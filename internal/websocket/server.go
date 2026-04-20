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

	"rtc-media-server/internal/config"
	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
)

const (
	inputAudioAppendEvent             = "input_audio_buffer.append"
	inputAudioCommitEvent             = "input_audio_buffer.commit"
	responseCreateEvent               = "response.create"
	responseCancelEvent               = "response.cancel"
	sessionUpdateEvent                = "session.update"
	sessionCreatedEvent               = "session.created"
	sessionUpdatedEvent               = "session.updated"
	speechStartedEvent                = "input_audio_buffer.speech_started"
	speechStoppedEvent                = "input_audio_buffer.speech_stopped"
	inputAudioCommittedEvent          = "input_audio_buffer.committed"
	responseCreatedEvent              = "response.created"
	responseAudioTranscriptDeltaEvent = "response.audio_transcript.delta"
	responseAudioDeltaType            = "response.audio.delta"
	responseDoneEvent                 = "response.done"
	errorEvent                        = "error"
)

// Server 是全局 WebSocket 监听器，负责 WSS 接入并创建每个客户端的 Connector。
type Server struct {
	cfg       config.WebSocketConfig
	callbacks Callbacks
	httpSrv   *http.Server
	clients   map[string]*clientConnector
	mu        sync.RWMutex
	started   bool
	stop      context.CancelFunc
}

// Callbacks 定义 WebSocket 连接生命周期事件的外部处理函数。
type Callbacks struct {
	OnConnect    func(ctx context.Context, client connector.ClientConnector) error
	OnDisconnect func(ctx context.Context, clientID string, err error)
	OnEvent      func(ctx context.Context, clientID string, event media.Event)
	OnRTT        func(ctx context.Context, clientID string, rtt time.Duration)
	OnError      func(ctx context.Context, clientID string, err error)
}

// clientConnector 表示某个客户端在 WebSocket 协议下的一组 channel 连接。
type clientConnector struct {
	id     string
	server *Server

	mu            sync.RWMutex
	stream        *channelConn
	streamPending bool
	audioOutput   connector.AudioOutput
	messageOutput connector.MessageOutput
	done          chan struct{}
	closeOnce     sync.Once
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

// NewServer 使用进程配置单例创建全局 WebSocket 监听器。
func NewServer(callbacks Callbacks) (*Server, error) {
	cfg := config.Get().WebSocket
	return NewServerWithConfig(cfg, callbacks)
}

// NewServerWithConfig 使用显式配置创建全局 WebSocket 监听器，主要供测试使用。
func NewServerWithConfig(cfg config.WebSocketConfig, callbacks Callbacks) (*Server, error) {
	if err := config.ValidateWebSocketConfig(cfg); err != nil {
		return nil, err
	}
	if err := config.ValidateWebSocketTLSFiles(cfg); err != nil {
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
		ReadHeaderTimeout: cfg.ReadTimeout.Duration(),
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.WriteTimeout.Duration())
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
	if err := client.SendMessage(r.Context(), connector.Message{
		SessionID: client.id,
		Direction: media.DirectionDownlink,
		Type:      connector.MessageSessionCreated,
	}); err != nil {
		s.reportError(r.Context(), clientID, fmt.Errorf("send session created: %w", err))
	}

	log.Infof("client_id=%s websocket channel connected protocol=%s channel=stream", clientID, client.Protocol())

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
		readCtx, cancel := context.WithTimeout(ctx, s.cfg.ReadTimeout.Duration())
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
		if sendErr := client.SendMessage(ctx, connector.Message{
			SessionID: client.id,
			Direction: media.DirectionDownlink,
			Type:      connector.MessageError,
			Payload:   []byte("invalid json: " + err.Error()),
		}); sendErr != nil {
			s.reportError(ctx, client.id, fmt.Errorf("send error event: %w", sendErr))
		}
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

// handleInputAudioCommit 处理端侧音频提交事件。
func (s *Server) handleInputAudioCommit(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Infof("client_id=%s websocket stream event received event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	client.pushMessage(ctx, event, connector.MessageInputCommit)
}

// handleResponseCreate 处理端侧显式创建回复事件。
func (s *Server) handleResponseCreate(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Infof("client_id=%s websocket stream event received event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	client.pushMessage(ctx, event, connector.MessageResponseCreate)
}

// handleResponseCancel 处理端侧取消回复事件。
func (s *Server) handleResponseCancel(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Infof("client_id=%s websocket stream event received event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	client.pushMessage(ctx, event, connector.MessageResponseCancel)
}

// handleSessionUpdate 处理端侧会话更新事件。
func (s *Server) handleSessionUpdate(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Infof("client_id=%s websocket stream event received event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	client.pushMessage(ctx, event, connector.MessageSessionUpdated)
	if err := client.SendMessage(ctx, connector.Message{
		SessionID: client.id,
		Direction: media.DirectionDownlink,
		Type:      connector.MessageSessionUpdated,
	}); err != nil {
		s.reportError(ctx, client.id, fmt.Errorf("send session updated: %w", err))
	}
}

// handleUnknownStreamEvent 处理当前未支持的端侧 stream 事件。
func (s *Server) handleUnknownStreamEvent(ctx context.Context, client *clientConnector, event streamEvent) {
	log.Warnf("client_id=%s websocket stream event ignored event_type=%s event_id=%s", client.id, event.Type, event.EventID)
	s.reportEvent(ctx, client.id, event)
	if err := client.SendMessage(ctx, connector.Message{
		SessionID: client.id,
		Direction: media.DirectionDownlink,
		Type:      connector.MessageError,
		Payload:   []byte("unsupported event type: " + event.Type),
	}); err != nil {
		s.reportError(ctx, client.id, fmt.Errorf("send error event: %w", err))
	}
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
		client.clearOutputs()
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
	ticker := time.NewTicker(s.cfg.RTTInterval.Duration())
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
				pingCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout.Duration())
				rtt, err := client.MeasureRTT(pingCtx)
				cancel()
				if err != nil {
					s.reportError(ctx, client.id, fmt.Errorf("measure rtt: %w", err))
					continue
				}
				if s.callbacks.OnRTT != nil {
					s.callbacks.OnRTT(ctx, client.id, rtt)
				}
			}
		}
	}
}

// Protocol 返回客户端 Connector 使用的协议名称。
func (client *clientConnector) Protocol() string { return "websocket" }

// ID 返回客户端硬件 ID。
func (client *clientConnector) ID() string { return client.id }

// BindAudioOutput 绑定客户端收到上行音频后要推送到的输出端。
func (client *clientConnector) BindAudioOutput(output connector.AudioOutput) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.audioOutput = output
	return nil
}

// BindMessageOutput 绑定客户端收到消息后要推送到的输出端。
func (client *clientConnector) BindMessageOutput(output connector.MessageOutput) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.messageOutput = output
	return nil
}

// SendAudio 把 pipeline 输出的 base64 音频帧打包成 JSON 后写回 stream 连接。
func (client *clientConnector) SendAudio(ctx context.Context, frame media.Frame) error {
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
	return nil
}

// SendMessage 把消息作为 JSON/text 直接写回 stream 连接。
func (client *clientConnector) SendMessage(ctx context.Context, msg connector.Message) error {
	payload, err := client.packConnectorMessage(msg)
	if err == nil {
		return client.sendStreamPayload(ctx, payload)
	}
	if len(msg.Payload) == 0 {
		return err
	}
	return client.sendStreamPayload(ctx, msg.Payload)
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
	rtt := time.Since(start)
	return rtt, nil
}

// Close 关闭客户端 Connector 持有的所有 channel。
func (client *clientConnector) Close(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "connector closed"
	}
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
	writeCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout.Duration())
	defer cancel()

	ch.writeMu.Lock()
	defer ch.writeMu.Unlock()
	return ch.conn.Write(writeCtx, msgType, payload)
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

// boundAudioOutput 返回当前客户端绑定的音频输出端。
func (client *clientConnector) boundAudioOutput() connector.AudioOutput {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.audioOutput
}

// pushUplinkFrame 把上行媒体帧转交给绑定的音频输出端。
func (client *clientConnector) pushUplinkFrame(ctx context.Context, frame media.Frame) {
	output := client.boundAudioOutput()
	if output == nil {
		client.server.reportError(ctx, client.id, errors.New("websocket audio output not bound"))
		return
	}
	if err := output.Push(ctx, frame); err != nil {
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

// packConnectorMessage 将内部中性消息打包成端侧 stream JSON。
func (client *clientConnector) packConnectorMessage(msg connector.Message) ([]byte, error) {
	switch msg.Type {
	case connector.MessageSessionCreated:
		return packSimpleStreamEvent(sessionCreatedEvent)
	case connector.MessageSessionUpdated:
		return packSimpleStreamEvent(sessionUpdatedEvent)
	case connector.MessageSpeechStarted:
		return packSimpleStreamEvent(speechStartedEvent)
	case connector.MessageSpeechStopped:
		return packSimpleStreamEvent(speechStoppedEvent)
	case connector.MessageInputCommit:
		return packSimpleStreamEvent(inputAudioCommittedEvent)
	case connector.MessageResponseCreate:
		return packSimpleStreamEvent(responseCreatedEvent)
	case connector.MessageResponseTextDelta:
		return packDeltaEvent(responseAudioTranscriptDeltaEvent, string(msg.Payload))
	case connector.MessageResponseDone:
		return packSimpleStreamEvent(responseDoneEvent)
	case connector.MessageError:
		return packErrorEvent(string(msg.Payload))
	default:
		return nil, fmt.Errorf("client_id=%s unsupported connector message type %s", client.id, msg.Type)
	}
}

// sendStreamEvent 向端侧发送不带额外 payload 的 stream 事件。
func (client *clientConnector) sendStreamEvent(ctx context.Context, eventType string) error {
	payload, err := packSimpleStreamEvent(eventType)
	if err != nil {
		return err
	}
	return client.sendStreamPayload(ctx, payload)
}

// sendDeltaEvent 向端侧发送带 delta 字段的 stream 事件。
func (client *clientConnector) sendDeltaEvent(ctx context.Context, eventType string, delta string) error {
	payload, err := packDeltaEvent(eventType, delta)
	if err != nil {
		return err
	}
	return client.sendStreamPayload(ctx, payload)
}

// sendErrorEvent 向端侧发送最小错误事件。
func (client *clientConnector) sendErrorEvent(ctx context.Context, message string) error {
	payload, err := packErrorEvent(message)
	if err != nil {
		return err
	}
	return client.sendStreamPayload(ctx, payload)
}

// packSimpleStreamEvent 将只有 type 字段的控制事件打包为 JSON。
func packSimpleStreamEvent(eventType string) ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
	}{Type: eventType})
}

// packDeltaEvent 将带 delta 字段的控制事件打包为 JSON。
func packDeltaEvent(eventType string, delta string) ([]byte, error) {
	return json.Marshal(struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}{Type: eventType, Delta: delta})
}

// packErrorEvent 将错误消息打包为端侧错误 JSON。
func packErrorEvent(message string) ([]byte, error) {
	return json.Marshal(struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{Type: errorEvent, Message: message})
}

// clearOutputs 清理客户端绑定的数据输出端。
func (client *clientConnector) clearOutputs() {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.audioOutput = nil
	client.messageOutput = nil
}

// boundMessageOutput 返回当前客户端绑定的消息输出端。
func (client *clientConnector) boundMessageOutput() connector.MessageOutput {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.messageOutput
}

// pushMessage 把 stream 控制事件转成中性消息并转交给绑定的消息输出端。
func (client *clientConnector) pushMessage(ctx context.Context, event streamEvent, messageType string) {
	output := client.boundMessageOutput()
	if output == nil {
		return
	}
	msg := connector.Message{
		SessionID: client.id,
		Direction: media.DirectionUplink,
		Type:      messageType,
		Payload:   append([]byte(nil), event.Raw...),
		Metadata: map[string]string{
			"event_id":   event.EventID,
			"event_type": event.Type,
			"source":     "websocket_stream",
		},
	}
	if err := output.PushMessage(ctx, msg); err != nil {
		client.server.reportError(ctx, client.id, err)
	}
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
