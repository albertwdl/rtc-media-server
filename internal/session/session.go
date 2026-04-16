package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"rtc-media-server/internal/application"
	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/controller"
	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/pipeline"
	"rtc-media-server/internal/pipeline/stages"
	audioenhancement "rtc-media-server/internal/pipeline/stages/audioenhancement"
	"rtc-media-server/internal/pipeline/stages/vad"
)

const (
	// DefaultCloseTimeout 是 Session 关闭流程默认等待时长。
	DefaultCloseTimeout = 3 * time.Second
)

const (
	inputAudioSpeechStartedEvent      = "input_audio_buffer.speech_started"
	inputAudioSpeechStoppedEvent      = "input_audio_buffer.speech_stopped"
	sessionCreatedEvent               = "session.created"
	sessionUpdateEvent                = "session.update"
	sessionUpdatedEvent               = "session.updated"
	inputAudioCommitEvent             = "input_audio_buffer.commit"
	inputAudioCommittedEvent          = "input_audio_buffer.committed"
	responseCreateEvent               = "response.create"
	responseCancelEvent               = "response.cancel"
	responseCreatedEvent              = "response.created"
	responseAudioTranscriptDeltaEvent = "response.audio_transcript.delta"
	transcriptionDoneEvent            = "conversation.item.input_audio_transcription.completed"
	responseDoneEvent                 = "response.done"
	errorEvent                        = "error"
)

const (
	mockResponseAudioDuration = 20 * time.Millisecond
	mockResponseSendGap       = 50 * time.Millisecond
	pcm16BytesPerSample       = 2
	mockTranscriptDelta       = "ok"
)

// Config 定义 Session 管理和固定链路运行所需的参数。
type Config struct {
	UplinkQueueSize   int
	DownlinkQueueSize int
	CloseTimeout      time.Duration
	TargetFormat      media.Format
	Controller        controller.Config
}

// Manager 管理所有业务 Session。
type Manager struct {
	cfg Config

	mu       sync.RWMutex
	sessions map[string]*Session
}

// Session 是业务会话核心，负责把 Connector 的数据输入输出和上下行 pipeline 串起来。
// Connector 负责连接收发，pipeline 负责媒体处理流。
type Session struct {
	id               string
	clientConnector  connector.ClientConnector
	serviceConnector connector.ServiceConnector
	controller       *controller.Controller

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	uplink   media.Pipeline
	downlink media.Pipeline

	mu             sync.RWMutex
	err            error
	rtt            time.Duration
	ok             bool
	responseCancel context.CancelFunc
	responseID     uint64
}

// NewManager 创建业务 Session 管理器。
func NewManager(cfg Config) *Manager {
	cfg = normalizeConfig(cfg)
	return &Manager{
		cfg:      cfg,
		sessions: make(map[string]*Session),
	}
}

// Attach 把一个客户端 Connector 挂到业务 Session 上。
// 如果该 Connector ID 已存在，则复用已有 Session。
func (m *Manager) Attach(ctx context.Context, client connector.ClientConnector) (*Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.sessions[client.ID()]
	if existing != nil {
		return existing, false, nil
	}

	s, err := NewSession(ctx, m.cfg, client)
	if err != nil {
		return nil, false, err
	}

	m.sessions[s.id] = s
	return s, true, nil
}

// NewSession 根据客户端 Connector 创建并启动一个完整 Session。
func NewSession(ctx context.Context, cfg Config, client connector.ClientConnector) (*Session, error) {
	cfg = normalizeConfig(cfg)
	sessionCtx, cancel := context.WithCancel(context.Background())
	s := &Session{
		id:              client.ID(),
		clientConnector: client,
		ctx:             sessionCtx,
		cancel:          cancel,
		done:            make(chan struct{}),
	}

	service := application.NewMockConnector(s.ID())
	s.serviceConnector = service
	cleanup := func(reason string) {
		cancel()
		if s.controller != nil {
			_ = s.controller.Close(ctx)
		}
		if s.uplink != nil {
			_ = s.uplink.Close(ctx)
		}
		if s.downlink != nil {
			_ = s.downlink.Close(ctx)
		}
		_ = service.Close(ctx, reason)
	}

	s.controller = controller.New(cfg.Controller, controller.Dependencies{
		SessionID: s.id,
		CloseSession: func(ctx context.Context, reason string) error {
			return s.Close(ctx, reason)
		},
		OnStageEvent: s.OnStageEvent,
	})

	engine, err := audioenhancement.NewMockStage("")
	if err != nil {
		cleanup("create audio enhancement failed")
		return nil, err
	}
	s.controller.RegisterReferenceConsumer(engine.Name(), engine)
	vadStage := vad.NewMockStageWithTimeouts(
		s.Controller().InitialSilenceTimeout(),
		s.Controller().SilenceTimeout(),
	)
	vadStage.SetEventEmitter(s.Controller().Emit)

	uplinkStages := []media.Stage{
		stages.NewBase64Decode(),
		stages.NewALawDecode(cfg.TargetFormat),
		engine,
		vadStage,
	}
	downlinkStages := []media.Stage{
		stages.NewPCM16Normalizer(cfg.TargetFormat),
		stages.NewReferenceTap(s.Controller().OnDownlinkReference),
		stages.NewALawEncode(),
		stages.NewBase64Encode(),
	}

	s.uplink = s.newPipeline("uplink", cfg.UplinkQueueSize, uplinkStages, media.OutputFunc(service.SendAudio))
	s.downlink = s.newPipeline("downlink", cfg.DownlinkQueueSize, downlinkStages, media.OutputFunc(client.SendAudio))

	if err := s.uplink.Start(sessionCtx); err != nil {
		cleanup("start uplink failed")
		return nil, err
	}
	if err := s.downlink.Start(sessionCtx); err != nil {
		cleanup("start downlink failed")
		return nil, err
	}
	if err := service.BindAudioOutput(media.InputFunc(func(ctx context.Context, frame media.Frame) error {
		return s.EnqueueDownlink(ctx, frame)
	})); err != nil {
		cleanup("bind service connector audio output failed")
		return nil, err
	}
	if err := service.BindMessageOutput(media.MessageOutputFunc(s.OnServiceMessage)); err != nil {
		cleanup("bind service connector message output failed")
		return nil, err
	}
	if err := client.BindAudioOutput(s.uplink); err != nil {
		cleanup("bind client connector audio output failed")
		return nil, err
	}
	if err := client.BindMessageOutput(media.MessageOutputFunc(s.OnClientMessage)); err != nil {
		cleanup("bind client connector message output failed")
		return nil, err
	}

	if err := s.sendSimpleEvent(sessionCtx, sessionCreatedEvent); err != nil {
		cleanup("send session created failed")
		return nil, err
	}

	log.Infof("client_id=%s session created protocol=%s", s.id, client.Protocol())
	return s, nil
}

// normalizeConfig 填充 Session 运行参数的默认值。
func normalizeConfig(cfg Config) Config {
	if cfg.UplinkQueueSize <= 0 {
		cfg.UplinkQueueSize = pipeline.DefaultQueueSize
	}
	if cfg.DownlinkQueueSize <= 0 {
		cfg.DownlinkQueueSize = pipeline.DefaultQueueSize
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = DefaultCloseTimeout
	}
	if cfg.TargetFormat.Kind == "" {
		cfg.TargetFormat = media.DefaultPCM16Format()
	}
	return cfg
}

// newPipeline 创建带 Session 错误回调的队列 pipeline。
func (s *Session) newPipeline(name string, queueSize int, stages []media.Stage, output media.Output) media.Pipeline {
	p := pipeline.NewQueuePipeline(name, queueSize, stages, output)
	p.SetErrorHandler(func(ctx context.Context, frame media.Frame, err error) {
		s.OnError(ctx, fmt.Errorf("%s pipeline: %w", name, err))
	})
	return p
}

// Remove 关闭并移除指定 Session。
func (m *Manager) Remove(ctx context.Context, id string, err error) {
	m.mu.Lock()
	s := m.sessions[id]
	if s != nil {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if s != nil {
		s.setErr(err)
		_ = s.Close(ctx, "session removed")
	}
}

// Close 关闭所有 Session。
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		sessions = append(sessions, s)
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		if err := s.Close(ctx, "manager closed"); err != nil {
			return err
		}
	}
	return nil
}

// Get 按 Session ID 查询当前在线 Session。
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.sessions[id]
	return s, s != nil
}

// ID 返回 Session 的客户端标识。
func (s *Session) ID() string { return s.id }

// Context 返回 Session 生命周期上下文。
func (s *Session) Context() context.Context { return s.ctx }

// Done 返回 Session 关闭通知。
func (s *Session) Done() <-chan struct{} { return s.done }

// ClientConnector 返回 Session 持有的客户端 Connector。
func (s *Session) ClientConnector() connector.ClientConnector { return s.clientConnector }

// ServiceConnector 返回 Session 持有的服务侧 Connector。
func (s *Session) ServiceConnector() connector.ServiceConnector { return s.serviceConnector }

// Controller 返回 Session 持有的跨管线 Controller。
func (s *Session) Controller() *controller.Controller { return s.controller }

// Uplink 返回 Session 的上行 pipeline。
func (s *Session) Uplink() media.Pipeline { return s.uplink }

// Downlink 返回 Session 的下行 pipeline。
func (s *Session) Downlink() media.Pipeline { return s.downlink }

// Err 返回 Session 关闭或移除时记录的错误。
func (s *Session) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

// EnqueueDownlink 把后端 TTS 或其他下行媒体投递到当前 Session 的下行 pipeline。
func (s *Session) EnqueueDownlink(ctx context.Context, frame media.Frame) error {
	frame.SessionID = s.id
	frame.Direction = media.DirectionDownlink
	if frame.Timestamp.IsZero() {
		frame.Timestamp = time.Now()
	}
	return s.downlink.Push(ctx, frame)
}

// MeasureRTT 通过客户端 Connector 主动测量 RTT 并缓存结果。
func (s *Session) MeasureRTT(ctx context.Context) (time.Duration, error) {
	rtt, err := s.clientConnector.MeasureRTT(ctx)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.rtt = rtt
	s.ok = true
	s.mu.Unlock()
	return rtt, nil
}

// RTT 返回最近一次成功测量的 RTT。
func (s *Session) RTT() (time.Duration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rtt, s.ok
}

// Close 按固定顺序关闭 Controller、pipeline 和 Connector。
func (s *Session) Close(ctx context.Context, reason string) error {
	var closeErr error
	s.once.Do(func() {
		s.cancel()
		s.cancelMockResponse()
		if s.controller != nil {
			_ = s.controller.Close(ctx)
		}
		if s.uplink != nil {
			_ = s.uplink.Close(ctx)
		}
		if s.downlink != nil {
			_ = s.downlink.Close(ctx)
		}
		if s.serviceConnector != nil {
			_ = s.serviceConnector.Close(ctx, reason)
		}
		if err := s.clientConnector.Close(ctx, reason); err != nil {
			closeErr = err
		}
		close(s.done)
		log.Infof("client_id=%s session closed reason=%s", s.id, reason)
	})
	return closeErr
}

// OnEvent 接收 Connector 上报的非媒体 stream 事件。
func (s *Session) OnEvent(ctx context.Context, event media.Event) {
	log.Infof("client_id=%s connector event event_type=%s bytes=%d", s.id, event.Type, len(event.Raw))
}

// OnStageEvent 接收 Controller 转发的跨管线事件。
func (s *Session) OnStageEvent(ctx context.Context, event media.StageEvent) {
	eventType, ok := stageEventToClientMessageType(event.Type)
	if !ok {
		return
	}
	if err := s.sendSimpleEvent(ctx, eventType); err != nil {
		s.OnError(ctx, fmt.Errorf("send stage event: %w", err))
	}
	if event.Type == controller.EventSpeechStopped {
		s.startMockResponse(ctx, true)
	}
}

// OnClientMessage 把端侧非媒体消息桥接到服务侧 Connector。
func (s *Session) OnClientMessage(ctx context.Context, msg media.Message) error {
	msg.SessionID = s.id
	msg.Direction = media.DirectionUplink
	if s.handleClientCommand(ctx, msg) {
		return nil
	}
	if s.serviceConnector == nil {
		return nil
	}
	return s.serviceConnector.SendMessage(ctx, msg)
}

// OnServiceMessage 把服务侧非媒体消息桥接到端侧 Connector。
func (s *Session) OnServiceMessage(ctx context.Context, msg media.Message) error {
	msg.SessionID = s.id
	msg.Direction = media.DirectionDownlink
	if s.clientConnector == nil {
		return nil
	}
	return s.clientConnector.SendMessage(ctx, msg)
}

// OnError 记录 Session 范围内的错误。
func (s *Session) OnError(ctx context.Context, err error) {
	log.Errorf("client_id=%s session error: %v", s.id, err)
}

// setErr 记录 Session 的最终错误状态。
func (s *Session) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// handleClientCommand 在 Session 内统一处理端侧控制指令。
func (s *Session) handleClientCommand(ctx context.Context, msg media.Message) bool {
	switch msg.Type {
	case "invalid_json":
		message := "invalid json"
		if msg.Metadata != nil && msg.Metadata["error"] != "" {
			message = message + ": " + msg.Metadata["error"]
		}
		if err := s.sendErrorEvent(ctx, message); err != nil {
			s.OnError(ctx, fmt.Errorf("send error event: %w", err))
		}
		return true
	case sessionUpdateEvent:
		if err := s.sendSimpleEvent(ctx, sessionUpdatedEvent); err != nil {
			s.OnError(ctx, fmt.Errorf("send session updated: %w", err))
		}
		return true
	case inputAudioCommitEvent:
		if err := s.sendSimpleEvent(ctx, inputAudioCommittedEvent); err != nil {
			s.OnError(ctx, fmt.Errorf("send input audio committed: %w", err))
		}
		return true
	case responseCreateEvent:
		s.startMockResponse(ctx, false)
		return true
	case responseCancelEvent:
		s.cancelMockResponse()
		if err := s.sendSimpleEvent(ctx, responseDoneEvent); err != nil {
			s.OnError(ctx, fmt.Errorf("send response done: %w", err))
		}
		return true
	default:
		if err := s.sendErrorEvent(ctx, "unsupported event type: "+msg.Type); err != nil {
			s.OnError(ctx, fmt.Errorf("send error event: %w", err))
		}
		return true
	}
}

// startMockResponse 启动一轮 Session 级模拟回复流程。
func (s *Session) startMockResponse(ctx context.Context, autoCommit bool) {
	s.mu.Lock()
	if s.responseCancel != nil {
		s.responseCancel()
	}
	s.responseID++
	responseID := s.responseID
	responseCtx, cancel := context.WithCancel(context.Background())
	s.responseCancel = cancel
	s.mu.Unlock()

	go func() {
		defer s.clearMockResponse(responseID)
		if err := s.runMockResponse(responseCtx, autoCommit); err != nil && !errors.Is(err, context.Canceled) {
			s.OnError(ctx, fmt.Errorf("run mock response: %w", err))
		}
	}()
}

// runMockResponse 按联调文档发送一轮最小模拟回复。
func (s *Session) runMockResponse(ctx context.Context, autoCommit bool) error {
	if autoCommit {
		if err := s.sendSimpleEvent(ctx, inputAudioCommittedEvent); err != nil {
			return err
		}
		if err := waitMockResponseGap(ctx); err != nil {
			return err
		}
	}
	if err := s.sendSimpleEvent(ctx, responseCreatedEvent); err != nil {
		return err
	}
	if err := waitMockResponseGap(ctx); err != nil {
		return err
	}
	if err := s.sendDeltaEvent(ctx, responseAudioTranscriptDeltaEvent, mockTranscriptDelta); err != nil {
		return err
	}
	if err := waitMockResponseGap(ctx); err != nil {
		return err
	}
	if err := s.EnqueueDownlink(ctx, media.Frame{
		Payload: makeSilentPCM(mockResponseAudioDuration),
		Format:  media.DefaultPCM16Format(),
		Metadata: map[string]string{
			"event_type": responseCreatedEvent,
			"source":     "session_mock_response",
		},
	}); err != nil {
		return err
	}
	if err := waitMockResponseGap(ctx); err != nil {
		return err
	}
	if err := s.sendTranscriptionDone(ctx, ""); err != nil {
		return err
	}
	if err := waitMockResponseGap(ctx); err != nil {
		return err
	}
	return s.sendSimpleEvent(ctx, responseDoneEvent)
}

// cancelMockResponse 停止当前正在运行的模拟回复。
func (s *Session) cancelMockResponse() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.responseCancel != nil {
		s.responseCancel()
		s.responseCancel = nil
	}
}

// clearMockResponse 清理指定模拟回复的取消函数。
func (s *Session) clearMockResponse(responseID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.responseID == responseID {
		s.responseCancel = nil
	}
}

// sendSimpleEvent 向端侧发送只包含 type 字段的事件。
func (s *Session) sendSimpleEvent(ctx context.Context, eventType string) error {
	payload, err := packSimpleMessageEvent(eventType)
	if err != nil {
		return err
	}
	return s.OnServiceMessage(ctx, media.Message{
		SessionID: s.id,
		Direction: media.DirectionDownlink,
		Type:      eventType,
		Payload:   payload,
	})
}

// sendDeltaEvent 向端侧发送带 delta 字段的事件。
func (s *Session) sendDeltaEvent(ctx context.Context, eventType string, delta string) error {
	payload, err := json.Marshal(struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}{Type: eventType, Delta: delta})
	if err != nil {
		return err
	}
	return s.OnServiceMessage(ctx, media.Message{
		SessionID: s.id,
		Direction: media.DirectionDownlink,
		Type:      eventType,
		Payload:   payload,
	})
}

// sendTranscriptionDone 向端侧发送模拟输入转写完成事件。
func (s *Session) sendTranscriptionDone(ctx context.Context, transcript string) error {
	payload, err := json.Marshal(struct {
		Type       string `json:"type"`
		Transcript string `json:"transcript"`
	}{Type: transcriptionDoneEvent, Transcript: transcript})
	if err != nil {
		return err
	}
	return s.OnServiceMessage(ctx, media.Message{
		SessionID: s.id,
		Direction: media.DirectionDownlink,
		Type:      transcriptionDoneEvent,
		Payload:   payload,
	})
}

// sendErrorEvent 向端侧发送最小错误事件。
func (s *Session) sendErrorEvent(ctx context.Context, message string) error {
	payload, err := json.Marshal(struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{Type: errorEvent, Message: message})
	if err != nil {
		return err
	}
	return s.OnServiceMessage(ctx, media.Message{
		SessionID: s.id,
		Direction: media.DirectionDownlink,
		Type:      errorEvent,
		Payload:   payload,
	})
}

// waitMockResponseGap 在模拟事件之间留出短间隔。
func waitMockResponseGap(ctx context.Context) error {
	timer := time.NewTimer(mockResponseSendGap)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// makeSilentPCM 生成用于联调的 PCM16LE 静音帧。
func makeSilentPCM(duration time.Duration) []byte {
	if duration <= 0 {
		duration = mockResponseAudioDuration
	}
	samples := int(duration.Seconds() * float64(media.DefaultAudioSampleRate) * float64(media.DefaultAudioChannels))
	if samples <= 0 {
		samples = 1
	}
	return make([]byte, samples*pcm16BytesPerSample)
}

// stageEventToClientMessageType 将内部 stage 事件映射为端侧 stream 事件类型。
func stageEventToClientMessageType(eventType string) (string, bool) {
	switch eventType {
	case controller.EventSpeechStarted:
		return inputAudioSpeechStartedEvent, true
	case controller.EventSpeechStopped:
		return inputAudioSpeechStoppedEvent, true
	default:
		return "", false
	}
}

// packSimpleMessageEvent 打包只包含 type 字段的下行消息。
func packSimpleMessageEvent(eventType string) ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
	}{Type: eventType})
}
