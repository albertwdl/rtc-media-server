package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/media"
)

const (
	EventSilenceTimeout = "silence_timeout"
	EventBargeIn        = "barge_in"

	ReferenceDropNewest = "drop_newest"
)

// Config 定义 Controller 的跨管线协调参数。
type Config struct {
	InitialSilenceTimeout time.Duration
	SilenceTimeout        time.Duration
	ReferenceQueueSize    int
	ReferenceDropPolicy   string
}

// Dependencies 定义 Controller 操作 Session 和 Connector 所需的外部能力。
type Dependencies struct {
	SessionID        string
	ClientConnector  connector.ClientConnector
	ServiceConnector connector.ServiceConnector
	Logger           *slog.Logger
	CloseSession     func(ctx context.Context, reason string) error
}

// Controller 负责处理跨管线事件、下行参考信号和资源协调。
type Controller struct {
	cfg  Config
	deps Dependencies

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu                 sync.RWMutex
	referenceConsumers map[string]media.ReferenceConsumer
	references         chan media.Frame
}

// New 创建每个 Session 独立持有的 Controller。
func New(cfg Config, deps Dependencies) *Controller {
	if cfg.InitialSilenceTimeout <= 0 {
		cfg.InitialSilenceTimeout = 15 * time.Second
	}
	if cfg.SilenceTimeout <= 0 {
		cfg.SilenceTimeout = 5 * time.Second
	}
	if cfg.ReferenceQueueSize <= 0 {
		cfg.ReferenceQueueSize = 16
	}
	if cfg.ReferenceDropPolicy != ReferenceDropNewest {
		cfg.ReferenceDropPolicy = ReferenceDropNewest
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{
		cfg:                cfg,
		deps:               deps,
		ctx:                ctx,
		cancel:             cancel,
		done:               make(chan struct{}),
		referenceConsumers: make(map[string]media.ReferenceConsumer),
		references:         make(chan media.Frame, cfg.ReferenceQueueSize),
	}
	go c.run()
	return c
}

// Emit 接收 stage 抛出的控制事件，并进行跨组件仲裁。
func (c *Controller) Emit(ctx context.Context, event media.StageEvent) {
	if event.SessionID == "" {
		event.SessionID = c.deps.SessionID
	}
	c.deps.Logger.Info(
		"client_id="+event.SessionID+" controller event",
		slog.String("client_id", event.SessionID),
		slog.String("event_type", event.Type),
		slog.String("direction", string(event.Direction)),
		slog.String("stage", event.Stage),
	)
	switch event.Type {
	case EventSilenceTimeout:
		_ = c.CloseSession(ctx, "silence timeout")
	case EventBargeIn:
		_ = c.FlushDownlink(ctx, "barge in")
	}
}

// OnDownlinkReference 接收下行参考信号副本，并异步分发给注册的 AEC reference consumer。
func (c *Controller) OnDownlinkReference(ctx context.Context, frame media.Frame) {
	frame.SessionID = c.deps.SessionID
	select {
	case <-c.done:
		return
	case c.references <- cloneFrame(frame):
	default:
		c.deps.Logger.Warn(
			"client_id="+c.deps.SessionID+" controller reference queue full",
			slog.String("client_id", c.deps.SessionID),
			slog.Int("queue_len", len(c.references)),
		)
	}
}

// RegisterReferenceConsumer 注册需要下行参考信号的 stage，例如 AEC。
func (c *Controller) RegisterReferenceConsumer(name string, consumer media.ReferenceConsumer) {
	if name == "" || consumer == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.referenceConsumers[name] = consumer
}

// FlushDownlink 清理下行输出，通常用于打断正在播放的服务侧音频。
func (c *Controller) FlushDownlink(ctx context.Context, reason string) error {
	if c.deps.ServiceConnector == nil {
		return nil
	}
	c.deps.Logger.Info(
		"client_id="+c.deps.SessionID+" controller flush downlink",
		slog.String("client_id", c.deps.SessionID),
		slog.String("reason", reason),
	)
	return c.deps.ServiceConnector.Flush(ctx, reason)
}

// CloseSession 触发 Session 标准关闭流程。
func (c *Controller) CloseSession(ctx context.Context, reason string) error {
	if c.deps.CloseSession == nil {
		return nil
	}
	c.deps.Logger.Info(
		"client_id="+c.deps.SessionID+" controller close session",
		slog.String("client_id", c.deps.SessionID),
		slog.String("reason", reason),
	)
	return c.deps.CloseSession(ctx, reason)
}

func (c *Controller) Close(ctx context.Context) error {
	c.once.Do(func() {
		c.cancel()
		close(c.done)
	})
	return nil
}

func (c *Controller) Done() <-chan struct{} { return c.done }

func (c *Controller) InitialSilenceTimeout() time.Duration {
	return c.cfg.InitialSilenceTimeout
}

func (c *Controller) SilenceTimeout() time.Duration {
	return c.cfg.SilenceTimeout
}

func (c *Controller) run() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case frame := <-c.references:
			c.dispatchReference(c.ctx, frame)
		}
	}
}

func (c *Controller) dispatchReference(ctx context.Context, frame media.Frame) {
	c.mu.RLock()
	consumers := make(map[string]media.ReferenceConsumer, len(c.referenceConsumers))
	for name, consumer := range c.referenceConsumers {
		consumers[name] = consumer
	}
	c.mu.RUnlock()
	for name, consumer := range consumers {
		if err := consumer.AddReference(ctx, frame); err != nil {
			c.deps.Logger.Error(
				"client_id="+c.deps.SessionID+" reference consumer failed",
				slog.String("client_id", c.deps.SessionID),
				slog.String("consumer", name),
				slog.Any("error", err),
			)
		}
	}
}

func cloneFrame(frame media.Frame) media.Frame {
	frame.Payload = append([]byte(nil), frame.Payload...)
	if frame.Metadata != nil {
		metadata := make(map[string]string, len(frame.Metadata))
		for key, value := range frame.Metadata {
			metadata[key] = value
		}
		frame.Metadata = metadata
	}
	return frame
}
