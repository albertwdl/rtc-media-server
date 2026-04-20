package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"rtc-media-server/internal/application"
	"rtc-media-server/internal/config"
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

	mu  sync.RWMutex
	err error
	rtt time.Duration
	ok  bool

	responseActive bool
}

// ConfigFromAppConfig 将应用配置转换为 Session 运行配置。
func ConfigFromAppConfig(cfg config.Config) Config {
	return Config{
		CloseTimeout: cfg.Session.CloseTimeout.Duration(),
		Controller: controller.Config{
			InitialSilenceTimeout: cfg.Controller.InitialSilenceTimeout.Duration(),
			SilenceTimeout:        cfg.Controller.SilenceTimeout.Duration(),
			ReferenceQueueSize:    cfg.Controller.ReferenceQueueSize,
		},
	}
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

	engine, err := audioenhancement.NewMockStage("", s.RTT)
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
	if err := service.BindMessageOutput(connector.MessageOutputFunc(s.OnServiceMessage)); err != nil {
		cleanup("bind service connector message output failed")
		return nil, err
	}
	if err := client.BindAudioOutput(s.uplink); err != nil {
		cleanup("bind client connector audio output failed")
		return nil, err
	}
	if err := client.BindMessageOutput(connector.MessageOutputFunc(s.OnClientMessage)); err != nil {
		cleanup("bind client connector message output failed")
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
	if err := s.ensureResponseCreated(ctx); err != nil {
		return err
	}
	return s.downlink.Push(ctx, frame)
}

// UpdateRTT 更新当前 Session 最近一次 RTT，供需要连接状态的 stage 读取。
func (s *Session) UpdateRTT(rtt time.Duration) {
	if rtt <= 0 {
		return
	}
	s.mu.Lock()
	s.rtt = rtt
	s.ok = true
	s.mu.Unlock()
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
	switch eventType {
	case connector.MessageSpeechStarted:
		if err := s.sendClientMessage(ctx, connector.MessageSpeechStarted, nil); err != nil {
			s.OnError(ctx, fmt.Errorf("send speech started: %w", err))
		}
	case connector.MessageSpeechStopped:
		if err := s.sendClientMessage(ctx, connector.MessageSpeechStopped, nil); err != nil {
			s.OnError(ctx, fmt.Errorf("send speech stopped: %w", err))
			return
		}
		if err := s.sendClientMessage(ctx, connector.MessageInputCommit, nil); err != nil {
			s.OnError(ctx, fmt.Errorf("send input committed: %w", err))
			return
		}
		if err := s.requestServiceResponse(ctx); err != nil {
			s.OnError(ctx, fmt.Errorf("request service response: %w", err))
		}
	}
}

// OnClientMessage 处理端侧非媒体控制消息。
func (s *Session) OnClientMessage(ctx context.Context, msg connector.Message) error {
	msg.SessionID = s.id
	msg.Direction = media.DirectionUplink
	switch msg.Type {
	case connector.MessageSessionUpdated:
		return nil
	case connector.MessageInputCommit:
		if err := s.sendClientMessage(ctx, connector.MessageInputCommit, nil); err != nil {
			return err
		}
		return s.requestServiceResponse(ctx)
	case connector.MessageResponseCreate:
		return s.requestServiceResponse(ctx)
	case connector.MessageResponseCancel:
		return s.serviceConnector.SendMessage(ctx, connector.Message{
			SessionID: s.id,
			Direction: media.DirectionUplink,
			Type:      connector.MessageResponseCancel,
		})
	default:
		log.Warnf("client_id=%s session ignored client message type=%s", s.id, msg.Type)
		return nil
	}
}

// OnServiceMessage 根据服务侧返回驱动端侧 ClientConnector。
func (s *Session) OnServiceMessage(ctx context.Context, msg connector.Message) error {
	msg.SessionID = s.id
	msg.Direction = media.DirectionDownlink
	switch msg.Type {
	case connector.MessageResponseTextDelta:
		if err := s.ensureResponseCreated(ctx); err != nil {
			return err
		}
		return s.sendClientMessage(ctx, connector.MessageResponseTextDelta, msg.Payload)
	case connector.MessageResponseDone:
		if err := s.sendClientMessage(ctx, connector.MessageResponseDone, nil); err != nil {
			return err
		}
		s.setResponseActive(false)
		return nil
	default:
		if len(msg.Payload) > 0 {
			return s.clientConnector.SendMessage(ctx, msg)
		}
		log.Warnf("client_id=%s session ignored service message type=%s", s.id, msg.Type)
		return nil
	}
}

// requestServiceResponse 通知服务侧基于当前输入生成回复。
func (s *Session) requestServiceResponse(ctx context.Context) error {
	if s.serviceConnector == nil {
		return nil
	}
	return s.serviceConnector.SendMessage(ctx, connector.Message{
		SessionID: s.id,
		Direction: media.DirectionUplink,
		Type:      connector.MessageResponseCreate,
	})
}

// ensureResponseCreated 确保本轮回复开始事件已经发给端侧。
func (s *Session) ensureResponseCreated(ctx context.Context) error {
	s.mu.Lock()
	if s.responseActive {
		s.mu.Unlock()
		return nil
	}
	s.responseActive = true
	s.mu.Unlock()
	return s.sendClientMessage(ctx, connector.MessageResponseCreate, nil)
}

// sendClientMessage 向客户端 Connector 发送中性非媒体消息。
func (s *Session) sendClientMessage(ctx context.Context, msgType string, payload []byte) error {
	return s.clientConnector.SendMessage(ctx, connector.Message{
		SessionID: s.id,
		Direction: media.DirectionDownlink,
		Type:      msgType,
		Payload:   payload,
	})
}

// setResponseActive 更新当前回复状态。
func (s *Session) setResponseActive(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseActive = active
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

// stageEventToClientMessageType 将内部 stage 事件映射为中性消息类型。
func stageEventToClientMessageType(eventType string) (string, bool) {
	switch eventType {
	case controller.EventSpeechStarted:
		return connector.MessageSpeechStarted, true
	case controller.EventSpeechStopped:
		return connector.MessageSpeechStopped, true
	default:
		return "", false
	}
}
