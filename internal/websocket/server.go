package websocket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	coderws "github.com/coder/websocket"

	"rtc-media-server/internal/media"
	"rtc-media-server/internal/session"
)

// Server 是全局 WebSocket 监听器，负责 WSS 接入并创建每个客户端的 Connector。
type Server struct {
	cfg     Config
	manager *session.Manager
	logger  *slog.Logger
	httpSrv *http.Server
	clients map[string]*clientConnector
	mu      sync.RWMutex
	started bool
	stop    context.CancelFunc
}

// clientConnector 表示某个客户端在 WebSocket 协议下的一组 channel 连接。
type clientConnector struct {
	id     string
	server *Server

	mu            sync.RWMutex
	stream        *channelConn
	streamPending bool
	session       *session.Session
	done          chan struct{}
	closeOnce     sync.Once
}

// channelConn 封装单个 WebSocket channel 及其写锁。
type channelConn struct {
	conn    *coderws.Conn
	writeMu sync.Mutex
}

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
		clients: make(map[string]*clientConnector),
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

// handleStream 处理 stream 路径的 WebSocket 握手，并把连接挂载到对应 Session。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	clientID := strings.TrimSpace(r.Header.Get(s.cfg.ClientIDHeader))
	if clientID == "" {
		http.Error(w, "missing websocket client id header "+s.cfg.ClientIDHeader, http.StatusBadRequest)
		return
	}

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

	session, _, err := s.manager.Attach(r.Context(), client)
	if err != nil {
		_ = conn.Close(coderws.StatusInternalError, "create session failed")
		s.unregisterClient(clientID, err)
		return
	}
	client.setSession(session)

	s.logger.Info(
		"client_id="+clientID+" websocket channel connected",
		slog.String("client_id", clientID),
		slog.String("protocol", client.Protocol()),
		slog.String("channel", "stream"),
	)

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

// handleStreamPayload 将 WebSocket payload 封装成上行 JSON 媒体帧并交给 Session。
func (s *Server) handleStreamPayload(ctx context.Context, client *clientConnector, payload []byte) {
	client.reportMedia(ctx, media.Frame{
		SessionID: client.id,
		Direction: media.DirectionUplink,
		Timestamp: time.Now(),
		Payload:   append([]byte(nil), payload...),
		Format: media.Format{
			Kind:       media.KindAudio,
			Codec:      media.CodecJSON,
			SampleRate: s.cfg.Stream.SampleRate,
			Channels:   s.cfg.Stream.Channels,
		},
		Metadata: map[string]string{
			"source": "websocket_stream",
		},
	})
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

// unregisterClient 注销客户端连接，并在连接完全断开时移除对应 Session。
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
		client.reportError(context.Background(), err)
	}
	if disconnected {
		client.closeDone()
		if sess := client.getSession(); sess != nil {
			s.manager.Remove(context.Background(), clientID, err)
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
					client.reportError(ctx, fmt.Errorf("measure rtt: %w", err))
				}
			}
		}
	}
}

// Protocol 返回客户端 Connector 使用的协议名称。
func (client *clientConnector) Protocol() string { return "websocket" }

// ID 返回客户端硬件 ID。
func (client *clientConnector) ID() string { return client.id }

// Start 启动客户端 Connector。
// WebSocket 读取循环由 Server 在握手成功后直接驱动，这里仅用于满足 ClientConnector 接口。
func (client *clientConnector) Start(ctx context.Context, sink media.Sink) error {
	return nil
}

// Consume 作为下行 sink，把 pipeline 输出的 JSON 帧写回 stream 连接。
func (client *clientConnector) Consume(ctx context.Context, frame media.Frame) error {
	ch := client.streamConn()
	if ch == nil {
		return fmt.Errorf("client_id=%s stream channel not connected", client.id)
	}
	if frame.Format.Codec != media.CodecJSON {
		return fmt.Errorf("client_id=%s websocket connector requires json frame, got %s", client.id, frame.Format.Codec)
	}
	return client.server.write(ctx, ch, coderws.MessageText, frame.Payload)
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

// setSession 记录客户端 Connector 关联的业务 Session。
func (client *clientConnector) setSession(session *session.Session) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.session = session
}

// getSession 返回客户端 Connector 关联的业务 Session。
func (client *clientConnector) getSession() *session.Session {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.session
}

// reportMedia 把上行媒体帧转交给关联的 Session。
func (client *clientConnector) reportMedia(ctx context.Context, frame media.Frame) {
	if session := client.getSession(); session != nil {
		session.OnMedia(ctx, frame)
	}
}

// reportError 把连接错误上报给 Session；Session 尚未创建时直接记录日志。
func (client *clientConnector) reportError(ctx context.Context, err error) {
	if session := client.getSession(); session != nil {
		session.OnError(ctx, err)
		return
	}
	client.server.logger.Error(
		"client_id="+client.id+" websocket connector error",
		slog.String("client_id", client.id),
		slog.Any("error", err),
	)
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
