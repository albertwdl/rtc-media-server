package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	coderws "github.com/coder/websocket"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
)

const (
	inputAudioAppendEvent    = "input_audio_buffer.append"
	inputAudioCommitEvent    = "input_audio_buffer.commit"
	responseCreateEvent      = "response.create"
	responseCancelEvent      = "response.cancel"
	sessionUpdateEvent       = "session.update"
	sessionCreatedEvent      = "session.created"
	sessionUpdatedEvent      = "session.updated"
	speechStartedEvent       = "input_audio_buffer.speech_started"
	speechStoppedEvent       = "input_audio_buffer.speech_stopped"
	inputAudioCommittedEvent = "input_audio_buffer.committed"
	responseCreatedEvent     = "response.created"
	responseAudioDeltaType   = "response.audio.delta"
	transcriptionDoneEvent   = "conversation.item.input_audio_transcription.completed"
	responseDoneEvent        = "response.done"
)

const (
	mockResponseAudioDuration = 20 * time.Millisecond
	mockResponseSendGap       = 50 * time.Millisecond
	pcm16BytesPerSample       = 2
)

// Server 是全局 WebSocket 监听器，负责 WSS 接入并创建每个客户端的 Connector。
type Server struct {
	cfg       Config
	callbacks Callbacks
	httpSrv   *http.Server
	clients   map[string]*clientConnector
	mu        sync.RWMutex
	started   bool
	stop      context.CancelFunc
}

// Callbacks 定义 WebSocket 连接生命周期事件的外部处理函数。
type Callbacks struct {
	OnConnect       func(ctx context.Context, client connector.ClientConnector) error
	OnDisconnect    func(ctx context.Context, clientID string, err error)
	OnEvent         func(ctx context.Context, clientID string, event media.Event)
	OnResponseAudio func(ctx context.Context, clientID string, frame media.Frame) error
	OnError         func(ctx context.Context, clientID string, err error)
}

// clientConnector 表示某个客户端在 WebSocket 协议下的一组 channel 连接。
type clientConnector struct {
	id     string
	server *Server

	mu            sync.RWMutex
	stream        *channelConn
	streamPending bool
	dataInput     media.Input
	done          chan struct{}
	closeOnce     sync.Once

	responseMu     sync.Mutex
	responseCancel context.CancelFunc
	responseID     uint64
	responseAudio  chan uint64
}

// channelConn 封装单个 WebSocket channel 及其写锁。
type channelConn struct {
	conn    *coderws.Conn
	writeMu sync.Mutex
}

// streamEvent 表示端侧通过 stream 通道发送的完整 JSON 事件。
type streamEvent struct {
	EventID string          `json:"event_id"`
	Type    string          `json:"type"`
	Audio   string          `json:"audio"`
	Session json.RawMessage `json:"session"`
	Raw     []byte          `json:"-"`
}

// NewServer 创建全局 WebSocket 监听器。
func NewServer(cfg Config, callbacks Callbacks) (*Server, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateTLSFiles(cfg); err != nil {
		return nil, err
	}

	s := &Server{
		cfg:       cfg,
		callbacks: callbacks,
		clients:   make(map[string]*clientConnector),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.StreamPath, s.handleStream)

	s.httpSrv = &http.Server{
		Addr:              net.JoinHostPort(cfg.Listen, fmt.Sprintf("%d", cfg.Port)),
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadTimeout,
	}

	return s, nil
}

// Start 启动 WSS 监听，并阻塞直到服务关闭或 ctx 被取消。
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("websocket server already started")
	}
	s.started = true
	s.stop = stop
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.started = false
		s.stop = nil
		s.mu.Unlock()
	}()

	go s.rttLoop(runCtx)
	go func() {
		<-runCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.WriteTimeout)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()

	err := s.httpSrv.ListenAndServeTLS(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅关闭监听器，并关闭所有 WebSocket 客户端连接。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.stop != nil {
		s.stop()
	}
	s.mu.Unlock()

	for _, client := range s.snapshotClients() {
		_ = client.Close(ctx, "server shutdown")
	}
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// handleStream 处理 stream 路径的 WebSocket 握手，并通过回调交给外部组装层。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	clientID := strings.TrimSpace(r.Header.Get(s.cfg.ClientIDHeader))
	if clientID == "" {
		http.Error(w, "missing websocket client id header "+s.cfg.ClientIDHeader, http.StatusBadRequest)
		return
	}
	log.Debugf(
		"client_id=%s websocket handshake path=%s raw_query=%s auth_type=%s device_name=%s instance_id=%s signature_present=%v",
		clientID,
		r.URL.Path,
		r.URL.RawQuery,
		r.Header.Get("X-Auth-Type"),
		r.Header.Get("X-Device-Name"),
		r.Header.Get("X-Instance-Id"),
		r.Header.Get("X-Signature") != "",
	)

	client, err := s.reserveClient(clientID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{InsecureSkipVerify: false})
	if err != nil {
		s.unregisterClient(clientID, err)
		return
	}
	conn.SetReadLimit(s.cfg.MaxMessageBytes)

	ch := &channelConn{conn: conn}
	client.setStream(ch)

	if s.callbacks.OnConnect != nil {
		err = s.callbacks.OnConnect(r.Context(), client)
	}
	if err != nil {
		_ = conn.Close(coderws.StatusInternalError, "create session failed")
		s.unregisterClient(clientID, err)
		return
	}

	log.Infof("client_id=%s websocket channel connected protocol=%s channel=stream", clientID, client.Protocol())
	if err := client.sendStreamEvent(r.Context(), sessionCreatedEvent); err != nil {
		s.reportError(r.Context(), clientID, fmt.Errorf("send session created: %w", err))
	}

	s.readStream(r.Context(), client, ch)
}

// readStream 持续读取 stream 连接中的业务 payload，并在退出时注销客户端连接。
func (s *Server) readStream(ctx context.Context, client *clientConnector, ch *channelConn) {
	var readErr error
	defer func() {
		_ = ch.conn.Close(coderws.StatusNormalClosure, "stream channel closed")
		s.unregisterClient(client.id, readErr)
	}()

	for {
		readCtx, cancel := context.WithTimeout(ctx, s.cfg.ReadTimeout)
		msgType, payload, err := ch.conn.Read(readCtx)
		cancel()
		if err != nil {
			readErr = err
			return
		}
		if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
			continue
		}
		s.handleStreamPayload(ctx, client, payload)
	}
}

// handleStreamPayload 解析 stream JSON 事件，并只把音频字段转成媒体帧交给 pipeline。
func (s *Server) handleStreamPayload(ctx context.Context, client *clientConnector, payload []byte) {
	log.Debugf("client_id=%s websocket received json=%s", client.id, string(payload))
	event, err := parseStreamEvent(payload)
	if err != nil {
		s.reportError(ctx, client.id, fmt.Errorf("parse stream event: %w", err))
		return
	}

	switch event.Type {
	case inputAudioAppendEvent:
		s.handleInputAudioAppend(ctx, client, event)
	case inputAudioCommitEvent:
		s.handleInputAudioCommit(ctx, client, event)
	case responseCreateEvent:
		s.handleResponseCreate(ctx, client, event)
	case responseCancelEvent:
		s.handleResponseCancel(ctx, client, event)
	case sessionUpdateEvent:
		s.handleSessionUpdate(ctx, client, event)
	default:
		s.handleUnknownStreamEvent(ctx, client, event)
	}
}

// parseStreamEvent 将 WebSocket payload 解析为 stream JSON 事件。
func parseStreamEvent(payload []byte) (streamEvent, error) {
	var event streamEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return event, err
	}
	event.Raw = append([]byte(nil), payload...)
	if strings.TrimSpace(event.Type) == "" {
		return event, errors.New("stream event type is empty")
	}
	return event, nil
}

// handleInputAudioAppend 处理端侧音频追加事件，并把 audio 字段推入上行 pipeline。
func (s *Server) handleInputAudioAppend(ctx context.Context, client *clientConnector, event streamEvent) {
	s.reportEvent(ctx, client.id, event)
	if event.Audio == "" {
		log.Warnf("client_id=%s websocket stream append event has empty audio event_id=%s", client.id, event.EventID)
		return
	}
	client.pushUplinkFrame(ctx, media.Frame{
		SessionID: client.id,
		Direction: media.DirectionUplink,
		Timestamp: time.Now(),
		Payload:   []byte(event.Audio),
		Format: media.Format{
			Kind:       media.KindAudio,
			Codec:      media.CodecBase64,
			SampleRate: s.cfg.Stream.SampleRate,
			Channels:   s.cfg.Stream.Channels,
		},
		Metadata: map[string]string{
			"event_id":   event.EventID,
			"event_type": event.Type,
			"source":     "websocket_stream",
		},
	})
}

// handleInputAudioCommit 预留 input_audio_buffer.commit 控制事件处理入口。
func (s *Server) handleInputAudioCommit(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Infof("client_id=%s websocket stream event received event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	if err := client.sendStreamEvent(ctx, inputAudioCommittedEvent); err != nil {
		s.reportError(ctx, client.id, fmt.Errorf("send input audio committed: %w", err))
	}
}

// handleResponseCreate 预留 response.create 控制事件处理入口。
func (s *Server) handleResponseCreate(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Infof("client_id=%s websocket stream event received event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	s.startMockResponse(ctx, client)
}

// handleResponseCancel 预留 response.cancel 控制事件处理入口。
func (s *Server) handleResponseCancel(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Infof("client_id=%s websocket stream event received event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	client.cancelMockResponse()
}

// handleSessionUpdate 预留 session.update 控制事件处理入口。
func (s *Server) handleSessionUpdate(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Infof("client_id=%s websocket stream event received event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	if err := client.sendStreamEvent(ctx, sessionUpdatedEvent); err != nil {
		s.reportError(ctx, client.id, fmt.Errorf("send session updated: %w", err))
	}
}

// handleUnknownStreamEvent 处理当前未识别的 stream 事件。
func (s *Server) handleUnknownStreamEvent(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Warnf("client_id=%s websocket stream event ignored event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
}

// reserveClient 为指定客户端预留 stream 连接，防止同一客户端重复建联。
func (s *Server) reserveClient(clientID string) (*clientConnector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	client := s.clients[clientID]
	if client == nil {
		client = &clientConnector{id: clientID, server: s, done: make(chan struct{})}
		s.clients[clientID] = client
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if client.stream != nil || client.streamPending {
		return nil, fmt.Errorf("websocket client %q stream channel already connected", clientID)
	}
	client.streamPending = true
	return client, nil
}

// unregisterClient 注销客户端连接，并在连接完全断开时触发断连回调。
func (s *Server) unregisterClient(clientID string, err error) {
	var (
		client       *clientConnector
		disconnected bool
	)

	s.mu.Lock()
	client = s.clients[clientID]
	if client != nil {
		client.mu.Lock()
		client.stream = nil
		client.streamPending = false
		delete(s.clients, clientID)
		disconnected = true
		client.mu.Unlock()
	}
	s.mu.Unlock()

	if client == nil {
		return
	}
	if err != nil && !isNormalClose(err) {
		s.reportError(context.Background(), clientID, err)
	}
	if disconnected {
		client.cancelMockResponse()
		client.clearDataInput()
		client.closeDone()
		if s.callbacks.OnDisconnect != nil {
			s.callbacks.OnDisconnect(context.Background(), clientID, err)
		}
	}
}

// snapshotClients 返回当前客户端 Connector 快照，避免遍历时长时间持有全局锁。
func (s *Server) snapshotClients() []*clientConnector {
	s.mu.RLock()
	defer s.mu.RUnlock()

	clients := make([]*clientConnector, 0, len(s.clients))
	for _, client := range s.clients {
		clients = append(clients, client)
	}
	return clients
}

// rttLoop 按配置周期测量所有在线 stream 连接的 RTT。
func (s *Server) rttLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.RTTInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, client := range s.snapshotClients() {
				if client.streamConn() == nil {
					continue
				}
				pingCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
				_, err := client.MeasureRTT(pingCtx)
				cancel()
				if err != nil {
					s.reportError(ctx, client.id, fmt.Errorf("measure rtt: %w", err))
				}
			}
		}
	}
}

// Protocol 返回客户端 Connector 使用的协议名称。
func (client *clientConnector) Protocol() string { return "websocket" }

// ID 返回客户端硬件 ID。
func (client *clientConnector) ID() string { return client.id }

// BindInput 绑定客户端收到上行数据后要推送到的 pipeline 输入端。
func (client *clientConnector) BindInput(input media.Input) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.dataInput = input
	return nil
}

// SendData 把 pipeline 输出的 base64 音频帧打包成 JSON 后写回 stream 连接。
func (client *clientConnector) SendData(ctx context.Context, frame media.Frame) error {
	if frame.Format.Codec != media.CodecBase64 {
		return fmt.Errorf("client_id=%s websocket connector requires base64 frame, got %s", client.id, frame.Format.Codec)
	}
	payload, err := packResponseAudioDelta(frame.Payload)
	if err != nil {
		return err
	}
	if err := client.sendStreamPayload(ctx, payload); err != nil {
		return err
	}
	client.markMockResponseAudioSent(frame)
	return nil
}

// sendStreamEvent 向端侧发送不带额外 payload 的 stream 控制事件。
func (client *clientConnector) sendStreamEvent(ctx context.Context, eventType string) error {
	payload, err := packSimpleStreamEvent(eventType)
	if err != nil {
		return err
	}
	return client.sendStreamPayload(ctx, payload)
}

// sendTranscriptionDone 向端侧发送一条模拟转写完成事件。
func (client *clientConnector) sendTranscriptionDone(ctx context.Context, transcript string) error {
	payload, err := packTranscriptionDone(transcript)
	if err != nil {
		return err
	}
	return client.sendStreamPayload(ctx, payload)
}

// sendStreamPayload 向 stream 连接写入完整 JSON payload。
func (client *clientConnector) sendStreamPayload(ctx context.Context, payload []byte) error {
	ch := client.streamConn()
	if ch == nil {
		return fmt.Errorf("client_id=%s stream channel not connected", client.id)
	}
	log.Debugf("client_id=%s websocket sending json=%s", client.id, string(payload))
	return client.server.write(ctx, ch, coderws.MessageText, payload)
}

// MeasureRTT 通过 stream 连接发送 WebSocket Ping 并返回往返耗时。
func (client *clientConnector) MeasureRTT(ctx context.Context) (time.Duration, error) {
	ch := client.streamConn()
	if ch == nil {
		return 0, fmt.Errorf("client_id=%s stream channel not connected", client.id)
	}
	start := time.Now()
	ch.writeMu.Lock()
	err := ch.conn.Ping(ctx)
	ch.writeMu.Unlock()
	if err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// Close 关闭客户端 Connector 持有的所有 channel。
func (client *clientConnector) Close(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "connector closed"
	}
	client.cancelMockResponse()
	for _, ch := range client.channels() {
		ch.writeMu.Lock()
		_ = ch.conn.Close(coderws.StatusNormalClosure, reason)
		ch.writeMu.Unlock()
	}
	client.closeDone()
	return nil
}

// Done 返回客户端 Connector 关闭通知。
func (client *clientConnector) Done() <-chan struct{} {
	return client.done
}

// write 串行写入指定 WebSocket channel，避免并发写破坏连接状态。
func (s *Server) write(ctx context.Context, ch *channelConn, msgType coderws.MessageType, payload []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
	defer cancel()

	ch.writeMu.Lock()
	defer ch.writeMu.Unlock()
	return ch.conn.Write(writeCtx, msgType, payload)
}

// startMockResponse 启动当前客户端的一次最小下行模拟回复流程。
func (s *Server) startMockResponse(ctx context.Context, client *clientConnector) {
	client.responseMu.Lock()
	if client.responseCancel != nil {
		client.responseCancel()
	}
	client.responseID++
	responseID := client.responseID
	responseCtx, cancel := context.WithCancel(context.Background())
	client.responseCancel = cancel
	client.responseAudio = make(chan uint64, 1)
	client.responseMu.Unlock()

	go func() {
		defer client.clearMockResponse(responseID)
		if err := s.runMockResponse(responseCtx, client, responseID); err != nil && !errors.Is(err, context.Canceled) {
			s.reportError(ctx, client.id, fmt.Errorf("run mock response: %w", err))
		}
	}()
}

// runMockResponse 按联调文档下发一轮最小模拟回复事件。
func (s *Server) runMockResponse(ctx context.Context, client *clientConnector, responseID uint64) error {
	events := []string{
		speechStartedEvent,
		speechStoppedEvent,
		inputAudioCommittedEvent,
		responseCreatedEvent,
	}
	for _, eventType := range events {
		if err := client.sendStreamEvent(ctx, eventType); err != nil {
			return err
		}
		if err := waitMockResponseGap(ctx); err != nil {
			return err
		}
	}

	if s.callbacks.OnResponseAudio != nil {
		frame := media.Frame{
			SessionID: client.id,
			Direction: media.DirectionDownlink,
			Timestamp: time.Now(),
			Payload:   makeSilentPCM(mockResponseAudioDuration),
			Format:    media.DefaultPCM16Format(),
			Metadata: map[string]string{
				"event_type":  responseAudioDeltaType,
				"response_id": fmt.Sprintf("%d", responseID),
				"source":      "websocket_mock_response",
			},
		}
		if err := s.callbacks.OnResponseAudio(ctx, client.id, frame); err != nil {
			return err
		}
		waitCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
		err := client.waitMockResponseAudio(waitCtx, responseID)
		cancel()
		if err != nil {
			return err
		}
	}

	if err := client.sendTranscriptionDone(ctx, ""); err != nil {
		return err
	}
	if err := waitMockResponseGap(ctx); err != nil {
		return err
	}
	return client.sendStreamEvent(ctx, responseDoneEvent)
}

// waitMockResponseGap 在模拟事件之间留出短间隔，避免控制事件跑到下行音频前面。
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

// waitMockResponseAudio 等待模拟音频真正写入 WebSocket 后再继续发送后续事件。
func (client *clientConnector) waitMockResponseAudio(ctx context.Context, responseID uint64) error {
	client.responseMu.Lock()
	audioSent := client.responseAudio
	client.responseMu.Unlock()
	if audioSent == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case sentID := <-audioSent:
		if sentID != responseID {
			return context.Canceled
		}
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

// setStream 记录客户端的 stream channel，并清除建联中的 pending 状态。
func (client *clientConnector) setStream(ch *channelConn) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.stream = ch
	client.streamPending = false
}

// streamConn 返回当前客户端的 stream channel。
func (client *clientConnector) streamConn() *channelConn {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.stream
}

// channels 返回当前客户端已建立的全部 channel。
func (client *clientConnector) channels() []*channelConn {
	client.mu.RLock()
	defer client.mu.RUnlock()

	var channels []*channelConn
	if client.stream != nil {
		channels = append(channels, client.stream)
	}
	return channels
}

// boundDataInput 返回当前客户端绑定的 pipeline 输入端。
func (client *clientConnector) boundDataInput() media.Input {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.dataInput
}

// pushUplinkFrame 把上行媒体帧转交给绑定的 pipeline 输入端。
func (client *clientConnector) pushUplinkFrame(ctx context.Context, frame media.Frame) {
	input := client.boundDataInput()
	if input == nil {
		client.server.reportError(ctx, client.id, errors.New("websocket data input not bound"))
		return
	}
	if err := input.Push(ctx, frame); err != nil {
		client.server.reportError(ctx, client.id, err)
	}
}

// reportError 把连接错误上报给外部回调；未配置回调时直接记录日志。
func (s *Server) reportError(ctx context.Context, clientID string, err error) {
	if s.callbacks.OnError != nil {
		s.callbacks.OnError(ctx, clientID, err)
		return
	}
	log.Errorf("client_id=%s websocket connector error: %v", clientID, err)
}

// reportEvent 把 stream 控制事件上报给外部组装层。
func (s *Server) reportEvent(ctx context.Context, clientID string, event streamEvent) {
	if s.callbacks.OnEvent == nil {
		return
	}
	fields := map[string]string{
		"event_id": event.EventID,
		"audio":    event.Audio,
	}
	if len(event.Session) > 0 {
		fields["session"] = string(event.Session)
	}
	s.callbacks.OnEvent(ctx, clientID, media.Event{
		Type:   event.Type,
		Raw:    append([]byte(nil), event.Raw...),
		Fields: fields,
	})
}

// packResponseAudioDelta 将 base64 音频 payload 打包成端侧期望的下行 JSON。
func packResponseAudioDelta(base64Audio []byte) ([]byte, error) {
	return json.Marshal(struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}{
		Type:  responseAudioDeltaType,
		Delta: string(base64Audio),
	})
}

// packSimpleStreamEvent 将只有 type 字段的控制事件打包为 JSON。
func packSimpleStreamEvent(eventType string) ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
	}{Type: eventType})
}

// packTranscriptionDone 将模拟转写结果打包成端侧下行 JSON。
func packTranscriptionDone(transcript string) ([]byte, error) {
	return json.Marshal(struct {
		Type       string `json:"type"`
		Transcript string `json:"transcript"`
	}{
		Type:       transcriptionDoneEvent,
		Transcript: transcript,
	})
}

// clearDataInput 清理客户端绑定的 pipeline 输入端。
func (client *clientConnector) clearDataInput() {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.dataInput = nil
}

// markMockResponseAudioSent 标记模拟回复音频已经写入 stream。
func (client *clientConnector) markMockResponseAudioSent(frame media.Frame) {
	if frame.Metadata["source"] != "websocket_mock_response" {
		return
	}
	responseID, ok := parseResponseID(frame.Metadata["response_id"])
	if !ok {
		return
	}
	client.responseMu.Lock()
	audioSent := client.responseAudio
	currentID := client.responseID
	client.responseMu.Unlock()
	if audioSent == nil || currentID != responseID {
		return
	}
	select {
	case audioSent <- responseID:
	default:
	}
}

// cancelMockResponse 停止当前正在运行的模拟回复流程。
func (client *clientConnector) cancelMockResponse() {
	client.responseMu.Lock()
	defer client.responseMu.Unlock()
	if client.responseCancel != nil {
		client.responseCancel()
		client.responseCancel = nil
	}
	client.responseAudio = nil
}

// clearMockResponse 清理已结束的模拟回复取消函数。
func (client *clientConnector) clearMockResponse(responseID uint64) {
	client.responseMu.Lock()
	defer client.responseMu.Unlock()
	if client.responseID == responseID {
		client.responseCancel = nil
		client.responseAudio = nil
	}
}

// parseResponseID 解析模拟回复 ID。
func parseResponseID(value string) (uint64, bool) {
	var id uint64
	if _, err := fmt.Sscanf(value, "%d", &id); err != nil {
		return 0, false
	}
	return id, true
}

// closeDone 关闭客户端 Connector 的 done channel，保证只关闭一次。
func (client *clientConnector) closeDone() {
	client.closeOnce.Do(func() {
		close(client.done)
	})
}

// isNormalClose 判断错误是否属于正常连接关闭。
func isNormalClose(err error) bool {
	var closeErr coderws.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code == coderws.StatusNormalClosure || closeErr.Code == coderws.StatusGoingAway
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed)
}
