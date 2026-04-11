package websocket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	coderws "github.com/coder/websocket"
)

// ServerCallbacks 定义服务端级别的回调。
// OnSession 在某个 client 的第一个通道连接成功后触发，外部通常在这里绑定该 session 的专属回调。
type ServerCallbacks struct {
	OnSession func(session *Session)
	OnError   func(err error)
}

// SessionCallbacks 定义绑定到单个客户端 Session 的回调。
// 回调参数中的 Session 可用于获取 clientID、发送数据、断连或查询 RTT。
type SessionCallbacks struct {
	OnStreamPCM    func(session *Session, pcm []byte)
	OnStreamEvent  func(session *Session, event StreamEvent)
	OnCommand      func(session *Session, payload []byte)
	OnDisconnected func(session *Session, err error)
	OnError        func(session *Session, err error)
}

// StreamEvent 表示 stream 通道收到并解析后的 JSON 事件。
// RawJSON 保留原始 payload，AudioBase64 用于 input_audio_buffer.append，DeltaBase64 用于 response.audio.delta。
type StreamEvent struct {
	Type        string
	RawJSON     []byte
	AudioBase64 string
	DeltaBase64 string
}

// Server 管理 WSS 监听器以及所有在线客户端 Session。
type Server struct {
	cfg       Config
	callbacks ServerCallbacks
	httpSrv   *http.Server

	mu       sync.RWMutex
	started  bool
	stop     context.CancelFunc
	sessions map[string]*Session
}

// Session 表示一个由 X-Instance-Id 标识的客户端会话。
// 同一个客户端的 stream 和 cmd 两条连接会绑定到同一个 Session。
type Session struct {
	server *Server
	id     string

	mu            sync.RWMutex
	stream        *channelConn
	cmd           *channelConn
	streamPending bool
	cmdPending    bool
	callbacks     SessionCallbacks
	rtt           time.Duration
	rttOK         bool
}

type channelConn struct {
	kind    channelKind
	conn    *coderws.Conn
	writeMu sync.Mutex
}

type channelKind string

const (
	streamChannel channelKind = "stream"
	cmdChannel    channelKind = "cmd"
)

// NewServer 根据配置和服务端回调创建 WebSocket 服务端。
func NewServer(cfg Config, callbacks ServerCallbacks) (*Server, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateTLSFiles(cfg); err != nil {
		return nil, err
	}

	s := &Server{
		cfg:       cfg,
		callbacks: callbacks,
		sessions:  make(map[string]*Session),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(cfg.StreamPath, s.handleStream)
	mux.HandleFunc(cfg.CmdPath, s.handleCmd)

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

// Shutdown 优雅关闭监听器，并关闭所有客户端 Session 的连接。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.stop != nil {
		s.stop()
	}
	s.mu.Unlock()

	sessions := s.snapshotSessions()
	for _, session := range sessions {
		session.close(ctx, coderws.StatusNormalClosure, "server shutdown")
	}

	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// Session 根据 clientID 获取当前在线的客户端 Session。
func (s *Server) Session(clientID string) (*Session, bool) {
	session := s.getSession(clientID)
	return session, session != nil
}

// Disconnect 主动断开指定客户端的所有通道。
func (s *Server) Disconnect(ctx context.Context, clientID string, reason string) error {
	session := s.getSession(clientID)
	if session == nil {
		return fmt.Errorf("websocket client %q not found", clientID)
	}
	return session.Disconnect(ctx, reason)
}

// SendStreamPCM 通过指定客户端的 stream 通道发送 PCM 数据。
func (s *Server) SendStreamPCM(ctx context.Context, clientID string, pcm []byte) error {
	session := s.getSession(clientID)
	if session == nil {
		return fmt.Errorf("websocket client %q not found", clientID)
	}
	return session.SendStreamPCM(ctx, pcm)
}

// SendCommand 通过指定客户端的 cmd 通道发送控制消息 JSON。
func (s *Server) SendCommand(ctx context.Context, clientID string, payload []byte) error {
	session := s.getSession(clientID)
	if session == nil {
		return fmt.Errorf("websocket client %q not found", clientID)
	}
	return session.SendCommand(ctx, payload)
}

// RTT 返回指定客户端最近一次通过 stream 通道测得的 RTT。
func (s *Server) RTT(clientID string) (time.Duration, bool) {
	session := s.getSession(clientID)
	if session == nil {
		return 0, false
	}
	return session.RTT()
}

// MeasureRTT 只使用指定客户端的 stream 通道执行一次 WebSocket Ping/Pong RTT 测量。
func (s *Server) MeasureRTT(ctx context.Context, clientID string) (time.Duration, error) {
	session := s.getSession(clientID)
	if session == nil {
		return 0, fmt.Errorf("websocket client %q not found", clientID)
	}
	return session.MeasureRTT(ctx)
}

// ID 返回客户端在握手 Header 中传入的实例 ID。
func (session *Session) ID() string {
	return session.id
}

// SetCallbacks 绑定当前 Session 的专属回调。
// 该方法可重复调用，后一次设置会整体替换前一次回调配置。
func (session *Session) SetCallbacks(callbacks SessionCallbacks) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.callbacks = callbacks
}

// SendStreamPCM 将 PCM S16LE 编码为 G.711 A-law，再编码为 base64。
// 最终通过 stream 通道发送 response.audio.delta JSON；WebSocket 帧头和 mask 由底层库处理。
func (session *Session) SendStreamPCM(ctx context.Context, pcm []byte) error {
	ch := session.channel(streamChannel)
	if ch == nil {
		return fmt.Errorf("websocket client %q stream channel not connected", session.id)
	}

	alaw := EncodePCMToALaw(pcm)
	payload, err := json.Marshal(struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}{
		Type:  "response.audio.delta",
		Delta: base64.StdEncoding.EncodeToString(alaw),
	})
	if err != nil {
		return err
	}
	return session.server.write(ctx, ch, coderws.MessageText, payload)
}

// SendCommand 通过当前 Session 的 cmd 通道发送控制消息 JSON。
func (session *Session) SendCommand(ctx context.Context, payload []byte) error {
	ch := session.channel(cmdChannel)
	if ch == nil {
		return fmt.Errorf("websocket client %q cmd channel not connected", session.id)
	}
	return session.server.write(ctx, ch, coderws.MessageText, payload)
}

// MeasureRTT 在当前 Session 的 stream 通道上发送 WebSocket Ping 并等待 Pong。
// 该方法不使用 cmd 通道；stream 未连接时会返回错误。
func (session *Session) MeasureRTT(ctx context.Context) (time.Duration, error) {
	ch := session.channel(streamChannel)
	if ch == nil {
		return 0, fmt.Errorf("websocket client %q stream channel not connected", session.id)
	}

	start := time.Now()
	ch.writeMu.Lock()
	err := ch.conn.Ping(ctx)
	ch.writeMu.Unlock()
	if err != nil {
		return 0, err
	}

	rtt := time.Since(start)
	session.mu.Lock()
	session.rtt = rtt
	session.rttOK = true
	session.mu.Unlock()
	return rtt, nil
}

// RTT 返回当前 Session 最近一次通过 stream 通道测得的 RTT。
func (session *Session) RTT() (time.Duration, bool) {
	session.mu.RLock()
	defer session.mu.RUnlock()
	return session.rtt, session.rttOK
}

// Disconnect 主动断开当前 Session 的所有通道。
func (session *Session) Disconnect(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "disconnected by server"
	}
	session.close(ctx, coderws.StatusNormalClosure, reason)
	return nil
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	s.handleWebSocket(w, r, streamChannel)
}

func (s *Server) handleCmd(w http.ResponseWriter, r *http.Request) {
	s.handleWebSocket(w, r, cmdChannel)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request, kind channelKind) {
	clientID := strings.TrimSpace(r.Header.Get(s.cfg.ClientIDHeader))
	if clientID == "" {
		http.Error(w, "missing websocket client id header "+s.cfg.ClientIDHeader, http.StatusBadRequest)
		return
	}

	session, firstChannel, err := s.reserveChannel(clientID, kind)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{
		InsecureSkipVerify: false,
	})
	if err != nil {
		s.unregisterChannel(clientID, kind, err)
		return
	}
	conn.SetReadLimit(s.cfg.MaxMessageBytes)

	ch := &channelConn{kind: kind, conn: conn}
	session.setChannel(kind, ch)
	if firstChannel && s.callbacks.OnSession != nil {
		s.callbacks.OnSession(session)
	}

	if kind == streamChannel {
		s.readStream(r.Context(), session, ch)
		return
	}
	s.readCmd(r.Context(), session, ch)
}

func (s *Server) readStream(ctx context.Context, session *Session, ch *channelConn) {
	var readErr error
	defer func() {
		_ = ch.conn.Close(coderws.StatusNormalClosure, "stream channel closed")
		s.unregisterChannel(session.id, streamChannel, readErr)
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
		s.handleStreamPayload(session, payload)
	}
}

func (s *Server) handleStreamPayload(session *Session, payload []byte) {
	event, err := decodeStreamEvent(payload)
	if err != nil {
		session.reportError(fmt.Errorf("decode stream json: %w", err))
		return
	}

	session.dispatchStreamEvent(event)
	if event.Type != "input_audio_buffer.append" || event.AudioBase64 == "" {
		return
	}

	// stream 通道的业务 payload 是 JSON；audio 字段才是 base64 编码后的 A-law 音频。
	alaw, err := base64.StdEncoding.DecodeString(event.AudioBase64)
	if err != nil {
		session.reportError(fmt.Errorf("decode stream audio base64: %w", err))
		return
	}
	pcm := DecodeALawToPCM(alaw)
	session.dispatchStreamPCM(pcm)
}

func (s *Server) readCmd(ctx context.Context, session *Session, ch *channelConn) {
	var readErr error
	defer func() {
		_ = ch.conn.Close(coderws.StatusNormalClosure, "cmd channel closed")
		s.unregisterChannel(session.id, cmdChannel, readErr)
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
		session.dispatchCommand(payload)
	}
}

func decodeStreamEvent(payload []byte) (StreamEvent, error) {
	var raw struct {
		Type  string `json:"type"`
		Audio string `json:"audio"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return StreamEvent{}, err
	}
	return StreamEvent{
		Type:        raw.Type,
		RawJSON:     append([]byte(nil), payload...),
		AudioBase64: raw.Audio,
		DeltaBase64: raw.Delta,
	}, nil
}

func (s *Server) reserveChannel(clientID string, kind channelKind) (*Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session := s.sessions[clientID]
	firstChannel := false
	if session == nil {
		session = &Session{server: s, id: clientID}
		s.sessions[clientID] = session
		firstChannel = true
	}

	session.mu.Lock()
	duplicate := session.channelLocked(kind) != nil || session.pendingLocked(kind)
	if duplicate {
		session.mu.Unlock()
		if firstChannel {
			delete(s.sessions, clientID)
		}
		return nil, false, fmt.Errorf("websocket client %q %s channel already connected", clientID, kind)
	}
	session.setPendingLocked(kind, true)
	session.mu.Unlock()

	return session, firstChannel, nil
}

func (s *Server) unregisterChannel(clientID string, kind channelKind, err error) {
	var (
		session      *Session
		disconnected bool
	)

	s.mu.Lock()
	session = s.sessions[clientID]
	if session != nil {
		session.mu.Lock()
		switch kind {
		case streamChannel:
			session.stream = nil
			session.streamPending = false
		case cmdChannel:
			session.cmd = nil
			session.cmdPending = false
		}
		if session.stream == nil && session.cmd == nil && !session.streamPending && !session.cmdPending {
			delete(s.sessions, clientID)
			disconnected = true
		}
		session.mu.Unlock()
	}
	s.mu.Unlock()

	if session == nil {
		return
	}
	if err != nil && !isNormalClose(err) {
		session.reportError(err)
	}
	if disconnected {
		session.dispatchDisconnected(err)
	}
}

func (session *Session) close(ctx context.Context, code coderws.StatusCode, reason string) {
	channels := session.channels()
	for _, ch := range channels {
		ch.writeMu.Lock()
		_ = ch.conn.Close(code, reason)
		ch.writeMu.Unlock()
	}
}

func (s *Server) write(ctx context.Context, ch *channelConn, msgType coderws.MessageType, payload []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
	defer cancel()

	ch.writeMu.Lock()
	defer ch.writeMu.Unlock()
	return ch.conn.Write(writeCtx, msgType, payload)
}

func (s *Server) rttLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.RTTInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, session := range s.snapshotSessions() {
				if session.channel(streamChannel) == nil {
					continue
				}
				pingCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
				_, err := session.MeasureRTT(pingCtx)
				cancel()
				if err != nil {
					session.reportError(fmt.Errorf("measure rtt: %w", err))
				}
			}
		}
	}
}

func (s *Server) snapshotSessions() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]*Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

func (s *Server) getSession(clientID string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[clientID]
}

func (s *Server) reportError(err error) {
	if s.callbacks.OnError != nil {
		s.callbacks.OnError(err)
	}
}

func (session *Session) setChannel(kind channelKind, ch *channelConn) {
	session.mu.Lock()
	defer session.mu.Unlock()
	switch kind {
	case streamChannel:
		session.stream = ch
		session.streamPending = false
	case cmdChannel:
		session.cmd = ch
		session.cmdPending = false
	}
}

func (session *Session) channel(kind channelKind) *channelConn {
	session.mu.RLock()
	defer session.mu.RUnlock()
	return session.channelLocked(kind)
}

func (session *Session) pendingLocked(kind channelKind) bool {
	switch kind {
	case streamChannel:
		return session.streamPending
	case cmdChannel:
		return session.cmdPending
	default:
		return false
	}
}

func (session *Session) setPendingLocked(kind channelKind, pending bool) {
	switch kind {
	case streamChannel:
		session.streamPending = pending
	case cmdChannel:
		session.cmdPending = pending
	}
}

func (session *Session) channelLocked(kind channelKind) *channelConn {
	switch kind {
	case streamChannel:
		return session.stream
	case cmdChannel:
		return session.cmd
	default:
		return nil
	}
}

func (session *Session) channels() []*channelConn {
	session.mu.RLock()
	defer session.mu.RUnlock()

	channels := make([]*channelConn, 0, 2)
	if session.stream != nil {
		channels = append(channels, session.stream)
	}
	if session.cmd != nil {
		channels = append(channels, session.cmd)
	}
	return channels
}

func (session *Session) callbacksSnapshot() SessionCallbacks {
	session.mu.RLock()
	defer session.mu.RUnlock()
	return session.callbacks
}

func (session *Session) dispatchStreamPCM(pcm []byte) {
	callbacks := session.callbacksSnapshot()
	if callbacks.OnStreamPCM != nil {
		callbacks.OnStreamPCM(session, pcm)
	}
}

func (session *Session) dispatchStreamEvent(event StreamEvent) {
	callbacks := session.callbacksSnapshot()
	if callbacks.OnStreamEvent != nil {
		callbacks.OnStreamEvent(session, event)
	}
}

func (session *Session) dispatchCommand(payload []byte) {
	callbacks := session.callbacksSnapshot()
	if callbacks.OnCommand != nil {
		callbacks.OnCommand(session, payload)
	}
}

func (session *Session) dispatchDisconnected(err error) {
	callbacks := session.callbacksSnapshot()
	if callbacks.OnDisconnected != nil {
		callbacks.OnDisconnected(session, err)
	}
}

func (session *Session) reportError(err error) {
	callbacks := session.callbacksSnapshot()
	if callbacks.OnError != nil {
		callbacks.OnError(session, err)
		return
	}
	session.server.reportError(fmt.Errorf("websocket client %q: %w", session.id, err))
}

func isNormalClose(err error) bool {
	var closeErr coderws.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code == coderws.StatusNormalClosure || closeErr.Code == coderws.StatusGoingAway
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed)
}
