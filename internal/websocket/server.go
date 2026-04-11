package websocket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	coderws "github.com/coder/websocket"

	"rtc-media-server/internal/endpoint"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/session"
)

// StreamEvent 表示 stream 通道收到并解析后的 JSON 事件。
// RawJSON 保留原始 payload，AudioBase64 用于 input_audio_buffer.append，DeltaBase64 用于 response.audio.delta。
type StreamEvent struct {
	Type        string
	RawJSON     []byte
	AudioBase64 string
	DeltaBase64 string
}

// Server 是全局 WebSocket 监听器，负责 WSS 接入并创建每个客户端的 Endpoint。
type Server struct {
	cfg     Config
	manager *session.Manager
	logger  *slog.Logger
	httpSrv *http.Server
	clients map[string]*clientEndpoint
	mu      sync.RWMutex
	started bool
	stop    context.CancelFunc
}

type clientEndpoint struct {
	id     string
	server *Server

	mu            sync.RWMutex
	stream        *channelConn
	cmd           *channelConn
	streamPending bool
	cmdPending    bool
	session       *session.Session
	done          chan struct{}
	closeOnce     sync.Once
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

// NewServer 创建全局 WebSocket 监听器。
func NewServer(cfg Config, manager *session.Manager, logger *slog.Logger) (*Server, error) {
	if manager == nil {
		return nil, errors.New("websocket session manager is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateTLSFiles(cfg); err != nil {
		return nil, err
	}

	s := &Server{
		cfg:     cfg,
		manager: manager,
		logger:  logger,
		clients: make(map[string]*clientEndpoint),
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

	client, err := s.reserveChannel(clientID, kind)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{InsecureSkipVerify: false})
	if err != nil {
		s.unregisterChannel(clientID, kind, err)
		return
	}
	conn.SetReadLimit(s.cfg.MaxMessageBytes)

	ch := &channelConn{kind: kind, conn: conn}
	client.setChannel(kind, ch)

	session, _, err := s.manager.Attach(r.Context(), client)
	if err != nil {
		_ = conn.Close(coderws.StatusInternalError, "create session failed")
		s.unregisterChannel(clientID, kind, err)
		return
	}
	client.setSession(session)

	s.logger.Info(
		"client_id="+clientID+" websocket channel connected",
		slog.String("client_id", clientID),
		slog.String("protocol", client.Protocol()),
		slog.String("channel", string(kind)),
	)

	if kind == streamChannel {
		s.readStream(r.Context(), client, ch)
		return
	}
	s.readCmd(r.Context(), client, ch)
}

func (s *Server) readStream(ctx context.Context, client *clientEndpoint, ch *channelConn) {
	var readErr error
	defer func() {
		_ = ch.conn.Close(coderws.StatusNormalClosure, "stream channel closed")
		s.unregisterChannel(client.id, streamChannel, readErr)
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

func (s *Server) handleStreamPayload(ctx context.Context, client *clientEndpoint, payload []byte) {
	event, err := decodeStreamEvent(payload)
	if err != nil {
		client.reportError(ctx, fmt.Errorf("decode stream json: %w", err))
		return
	}

	client.reportEvent(ctx, endpoint.Event{
		Type: event.Type,
		Raw:  append([]byte(nil), payload...),
		Fields: map[string]string{
			"audio": event.AudioBase64,
			"delta": event.DeltaBase64,
		},
	})
	if event.Type != "input_audio_buffer.append" || event.AudioBase64 == "" {
		return
	}

	alaw, err := base64.StdEncoding.DecodeString(event.AudioBase64)
	if err != nil {
		client.reportError(ctx, fmt.Errorf("decode stream audio base64: %w", err))
		return
	}
	pcm := DecodeALawToPCM(alaw)
	client.reportMedia(ctx, media.Frame{
		SessionID: client.id,
		Direction: media.DirectionUplink,
		Timestamp: time.Now(),
		Payload:   pcm,
		Format:    media.DefaultPCM16Format(),
		Metadata: map[string]string{
			"source_event": event.Type,
		},
	})
}

func (s *Server) readCmd(ctx context.Context, client *clientEndpoint, ch *channelConn) {
	var readErr error
	defer func() {
		_ = ch.conn.Close(coderws.StatusNormalClosure, "cmd channel closed")
		s.unregisterChannel(client.id, cmdChannel, readErr)
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
		client.reportCommand(ctx, payload)
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

func (s *Server) reserveChannel(clientID string, kind channelKind) (*clientEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	client := s.clients[clientID]
	if client == nil {
		client = &clientEndpoint{id: clientID, server: s, done: make(chan struct{})}
		s.clients[clientID] = client
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if client.channelLocked(kind) != nil || client.pendingLocked(kind) {
		return nil, fmt.Errorf("websocket client %q %s channel already connected", clientID, kind)
	}
	client.setPendingLocked(kind, true)
	return client, nil
}

func (s *Server) unregisterChannel(clientID string, kind channelKind, err error) {
	var (
		client       *clientEndpoint
		disconnected bool
	)

	s.mu.Lock()
	client = s.clients[clientID]
	if client != nil {
		client.mu.Lock()
		switch kind {
		case streamChannel:
			client.stream = nil
			client.streamPending = false
		case cmdChannel:
			client.cmd = nil
			client.cmdPending = false
		}
		if client.stream == nil && client.cmd == nil && !client.streamPending && !client.cmdPending {
			delete(s.clients, clientID)
			disconnected = true
		}
		client.mu.Unlock()
	}
	s.mu.Unlock()

	if client == nil {
		return
	}
	if err != nil && !isNormalClose(err) {
		client.reportError(context.Background(), err)
	}
	if disconnected {
		client.closeDone()
		if sess := client.getSession(); sess != nil {
			s.manager.Remove(context.Background(), clientID, err)
		}
	}
}

func (s *Server) snapshotClients() []*clientEndpoint {
	s.mu.RLock()
	defer s.mu.RUnlock()

	clients := make([]*clientEndpoint, 0, len(s.clients))
	for _, client := range s.clients {
		clients = append(clients, client)
	}
	return clients
}

func (s *Server) rttLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.RTTInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, client := range s.snapshotClients() {
				if client.channel(streamChannel) == nil {
					continue
				}
				pingCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
				_, err := client.MeasureRTT(pingCtx)
				cancel()
				if err != nil {
					client.reportError(ctx, fmt.Errorf("measure rtt: %w", err))
				}
			}
		}
	}
}

func (client *clientEndpoint) Protocol() string { return "websocket" }

func (client *clientEndpoint) ID() string { return client.id }

func (client *clientEndpoint) Start(ctx context.Context, sink media.Sink) error {
	return nil
}

func (client *clientEndpoint) Consume(ctx context.Context, frame media.Frame) error {
	ch := client.channel(streamChannel)
	if ch == nil {
		return fmt.Errorf("client_id=%s stream channel not connected", client.id)
	}

	alaw := EncodePCMToALaw(frame.Payload)
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
	return client.server.write(ctx, ch, coderws.MessageText, payload)
}

func (client *clientEndpoint) SendCommand(ctx context.Context, payload []byte) error {
	ch := client.channel(cmdChannel)
	if ch == nil {
		return fmt.Errorf("client_id=%s cmd channel not connected", client.id)
	}
	return client.server.write(ctx, ch, coderws.MessageText, payload)
}

func (client *clientEndpoint) MeasureRTT(ctx context.Context) (time.Duration, error) {
	ch := client.channel(streamChannel)
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

func (client *clientEndpoint) Close(ctx context.Context, reason string) error {
	if reason == "" {
		reason = "endpoint closed"
	}
	for _, ch := range client.channels() {
		ch.writeMu.Lock()
		_ = ch.conn.Close(coderws.StatusNormalClosure, reason)
		ch.writeMu.Unlock()
	}
	client.closeDone()
	return nil
}

func (client *clientEndpoint) Done() <-chan struct{} {
	return client.done
}

func (s *Server) write(ctx context.Context, ch *channelConn, msgType coderws.MessageType, payload []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
	defer cancel()

	ch.writeMu.Lock()
	defer ch.writeMu.Unlock()
	return ch.conn.Write(writeCtx, msgType, payload)
}

func (client *clientEndpoint) setChannel(kind channelKind, ch *channelConn) {
	client.mu.Lock()
	defer client.mu.Unlock()
	switch kind {
	case streamChannel:
		client.stream = ch
		client.streamPending = false
	case cmdChannel:
		client.cmd = ch
		client.cmdPending = false
	}
}

func (client *clientEndpoint) channel(kind channelKind) *channelConn {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.channelLocked(kind)
}

func (client *clientEndpoint) channelLocked(kind channelKind) *channelConn {
	switch kind {
	case streamChannel:
		return client.stream
	case cmdChannel:
		return client.cmd
	default:
		return nil
	}
}

func (client *clientEndpoint) pendingLocked(kind channelKind) bool {
	switch kind {
	case streamChannel:
		return client.streamPending
	case cmdChannel:
		return client.cmdPending
	default:
		return false
	}
}

func (client *clientEndpoint) setPendingLocked(kind channelKind, pending bool) {
	switch kind {
	case streamChannel:
		client.streamPending = pending
	case cmdChannel:
		client.cmdPending = pending
	}
}

func (client *clientEndpoint) channels() []*channelConn {
	client.mu.RLock()
	defer client.mu.RUnlock()

	channels := make([]*channelConn, 0, 2)
	if client.stream != nil {
		channels = append(channels, client.stream)
	}
	if client.cmd != nil {
		channels = append(channels, client.cmd)
	}
	return channels
}

func (client *clientEndpoint) setSession(session *session.Session) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.session = session
}

func (client *clientEndpoint) getSession() *session.Session {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.session
}

func (client *clientEndpoint) reportMedia(ctx context.Context, frame media.Frame) {
	if session := client.getSession(); session != nil {
		session.OnMedia(ctx, frame)
	}
}

func (client *clientEndpoint) reportCommand(ctx context.Context, payload []byte) {
	if session := client.getSession(); session != nil {
		session.OnCommand(ctx, payload)
	}
}

func (client *clientEndpoint) reportEvent(ctx context.Context, event endpoint.Event) {
	if session := client.getSession(); session != nil {
		session.OnEvent(ctx, event)
	}
}

func (client *clientEndpoint) reportError(ctx context.Context, err error) {
	if session := client.getSession(); session != nil {
		session.OnError(ctx, err)
		return
	}
	client.server.logger.Error(
		"client_id="+client.id+" websocket endpoint error",
		slog.String("client_id", client.id),
		slog.Any("error", err),
	)
}

func (client *clientEndpoint) closeDone() {
	client.closeOnce.Do(func() {
		close(client.done)
	})
}

func isNormalClose(err error) bool {
	var closeErr coderws.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code == coderws.StatusNormalClosure || closeErr.Code == coderws.StatusGoingAway
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed)
}
