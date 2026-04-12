package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/controller"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/pipeline"
)

// Config 定义 Session 与双向 pipeline 的运行参数。
type Config struct {
	UplinkQueueSize   int
	DownlinkQueueSize int
	CloseTimeout      time.Duration
	TargetFormat      media.Format
	Controller        controller.Config
}

// Dependencies 定义 Session 层依赖的处理 stage 和业务输出 sink。
type Dependencies struct {
	UplinkStages        []media.Stage
	DownlinkStages      []media.Stage
	NewUplinkStages     func(session *Session) ([]media.Stage, error)
	NewDownlinkStages   func(session *Session) ([]media.Stage, error)
	NewServiceConnector func(session *Session) (connector.ServiceConnector, error)
	Logger              *slog.Logger
	OnSession           func(session *Session)
	OnEvent             func(session *Session, event media.Event)
	OnError             func(session *Session, err error)
}

// Manager 管理所有业务 Session。
type Manager struct {
	cfg  Config
	deps Dependencies

	mu       sync.RWMutex
	sessions map[string]*Session
}

// Session 是业务会话核心，持有 Connector、Controller 和上下行两条 pipeline。
type Session struct {
	id               string
	clientConnector  connector.ClientConnector
	serviceConnector connector.ServiceConnector
	controller       *controller.Controller
	logger           *slog.Logger

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

	onEvent func(session *Session, event media.Event)
	onError func(session *Session, err error)
}

// NewManager 创建业务 Session 管理器。
func NewManager(cfg Config, deps Dependencies) *Manager {
	if cfg.UplinkQueueSize <= 0 {
		cfg.UplinkQueueSize = 32
	}
	if cfg.DownlinkQueueSize <= 0 {
		cfg.DownlinkQueueSize = 32
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = 3 * time.Second
	}
	if cfg.TargetFormat.Kind == "" {
		cfg.TargetFormat = media.DefaultPCM16Format()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Manager{
		cfg:      cfg,
		deps:     deps,
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

	sessionCtx, cancel := context.WithCancel(context.Background())
	logger := m.deps.Logger.With(
		slog.String("client_id", client.ID()),
		slog.String("protocol", client.Protocol()),
	)
	s := &Session{
		id:              client.ID(),
		clientConnector: client,
		logger:          logger,
		ctx:             sessionCtx,
		cancel:          cancel,
		done:            make(chan struct{}),
		onEvent:         m.deps.OnEvent,
		onError:         m.deps.OnError,
	}

	service, err := m.serviceConnectorFor(s)
	if err != nil {
		cancel()
		return nil, false, err
	}
	s.serviceConnector = service
	cleanup := func(reason string) {
		cancel()
		if s.uplink != nil {
			_ = s.uplink.Close(ctx)
		}
		if s.downlink != nil {
			_ = s.downlink.Close(ctx)
		}
		if s.controller != nil {
			_ = s.controller.Close(ctx)
		}
		_ = service.Close(ctx, reason)
	}

	ctrl, err := m.controllerFor(s)
	if err != nil {
		cleanup("create controller failed")
		return nil, false, err
	}
	s.controller = ctrl

	uplinkStages, err := m.uplinkStagesFor(s)
	if err != nil {
		cleanup("create uplink stages failed")
		return nil, false, err
	}
	downlinkStages, err := m.downlinkStagesFor(s)
	if err != nil {
		cleanup("create downlink stages failed")
		return nil, false, err
	}

	s.uplink = s.newPipeline("uplink", m.cfg.UplinkQueueSize, uplinkStages, service)
	s.downlink = s.newPipeline("downlink", m.cfg.DownlinkQueueSize, downlinkStages, media.SinkFunc(func(ctx context.Context, frame media.Frame) error {
		return client.Consume(ctx, frame)
	}))

	if err := s.uplink.Start(sessionCtx); err != nil {
		cleanup("start uplink failed")
		return nil, false, err
	}
	if err := s.downlink.Start(sessionCtx); err != nil {
		cleanup("start downlink failed")
		return nil, false, err
	}
	if err := service.Start(sessionCtx, media.SinkFunc(func(ctx context.Context, frame media.Frame) error {
		return s.EnqueueDownlink(ctx, frame)
	})); err != nil {
		cleanup("start service connector failed")
		return nil, false, err
	}
	if err := client.Start(sessionCtx, media.SinkFunc(func(ctx context.Context, frame media.Frame) error {
		s.OnMedia(ctx, frame)
		return nil
	})); err != nil {
		cleanup("start client connector failed")
		return nil, false, err
	}

	m.sessions[s.id] = s
	if m.deps.OnSession != nil {
		m.deps.OnSession(s)
	}
	logger.Info("client_id=" + s.id + " session created")
	return s, true, nil
}

// newPipeline 创建带 Session 错误回调的队列 pipeline。
func (s *Session) newPipeline(name string, queueSize int, stages []media.Stage, sink media.Sink) media.Pipeline {
	p := pipeline.NewQueuePipeline(name, queueSize, stages, sink, s.logger)
	p.SetErrorHandler(func(ctx context.Context, frame media.Frame, err error) {
		s.OnError(ctx, fmt.Errorf("%s pipeline: %w", name, err))
	})
	return p
}

// serviceConnectorFor 为 Session 创建必需的服务侧 Connector。
func (m *Manager) serviceConnectorFor(s *Session) (connector.ServiceConnector, error) {
	if m.deps.NewServiceConnector == nil {
		return nil, errors.New("session NewServiceConnector is required")
	}
	return m.deps.NewServiceConnector(s)
}

// controllerFor 为 Session 创建独立 Controller。
func (m *Manager) controllerFor(s *Session) (*controller.Controller, error) {
	deps := controller.Dependencies{
		SessionID: s.id,
		Logger:    s.logger,
		CloseSession: func(ctx context.Context, reason string) error {
			return s.Close(ctx, reason)
		},
	}
	return controller.New(m.cfg.Controller, deps), nil
}

// uplinkStagesFor 为 Session 创建独立的上行 stage chain。
func (m *Manager) uplinkStagesFor(s *Session) ([]media.Stage, error) {
	if m.deps.NewUplinkStages != nil {
		return m.deps.NewUplinkStages(s)
	}
	return append([]media.Stage(nil), m.deps.UplinkStages...), nil
}

// downlinkStagesFor 为 Session 创建独立的下行 stage chain。
func (m *Manager) downlinkStagesFor(s *Session) ([]media.Stage, error) {
	if m.deps.NewDownlinkStages != nil {
		return m.deps.NewDownlinkStages(s)
	}
	return append([]media.Stage(nil), m.deps.DownlinkStages...), nil
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
	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.RUnlock()

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

// Logger 返回带 Session 字段的日志器。
func (s *Session) Logger() *slog.Logger { return s.logger }

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
		if s.controller != nil {
			_ = s.controller.Close(ctx)
		}
		_ = s.uplink.Close(ctx)
		_ = s.downlink.Close(ctx)
		if s.serviceConnector != nil {
			_ = s.serviceConnector.Close(ctx, reason)
		}
		if err := s.clientConnector.Close(ctx, reason); err != nil {
			closeErr = err
		}
		close(s.done)
		s.logger.Info("client_id="+s.id+" session closed", slog.String("reason", reason))
	})
	return closeErr
}

// OnMedia 接收客户端 Connector 上报的媒体帧并投递到上行 pipeline。
func (s *Session) OnMedia(ctx context.Context, frame media.Frame) {
	frame.SessionID = s.id
	frame.Direction = media.DirectionUplink
	if frame.Timestamp.IsZero() {
		frame.Timestamp = time.Now()
	}
	if err := s.uplink.Push(ctx, frame); err != nil {
		s.OnError(ctx, fmt.Errorf("push uplink frame: %w", err))
	}
}

// OnEvent 接收 Connector 或协议适配 stage 上报的非媒体事件。
func (s *Session) OnEvent(ctx context.Context, event media.Event) {
	s.logger.Info("client_id="+s.id+" connector event", slog.String("event_type", event.Type), slog.Int("bytes", len(event.Raw)))
	if s.onEvent != nil {
		s.onEvent(s, event)
	}
}

// OnError 记录并转发 Session 范围内的错误。
func (s *Session) OnError(ctx context.Context, err error) {
	s.logger.Error("client_id="+s.id+" session error", slog.Any("error", err))
	if s.onError != nil {
		s.onError(s, err)
	}
}

// setErr 记录 Session 的最终错误状态。
func (s *Session) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}
