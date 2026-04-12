package controller

import (
	"context"
	"sync"
	"time"

	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
)

const (
	EventSilenceTimeout = "silence_timeout"
)

const (
	// DefaultInitialSilenceTimeout 是连接建立后的首次静音等待时长。
	DefaultInitialSilenceTimeout = 15 * time.Second
	// DefaultSilenceTimeout 是首次有效语音后的静音等待时长。
	DefaultSilenceTimeout = 5 * time.Second
	// DefaultReferenceQueueSize 是下行参考信号默认队列长度。
	DefaultReferenceQueueSize = 16
)

// Config 定义 Controller 的跨管线协调参数。
type Config struct {
	InitialSilenceTimeout time.Duration
	SilenceTimeout        time.Duration
	ReferenceQueueSize    int
}

// Dependencies 定义 Controller 操作 Session 所需的外部能力。
type Dependencies struct {
	SessionID    string
	CloseSession func(ctx context.Context, reason string) error
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
		cfg.InitialSilenceTimeout = DefaultInitialSilenceTimeout
	}
	if cfg.SilenceTimeout <= 0 {
		cfg.SilenceTimeout = DefaultSilenceTimeout
	}
	if cfg.ReferenceQueueSize <= 0 {
		cfg.ReferenceQueueSize = DefaultReferenceQueueSize
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
	log.Infof(
		"client_id=%s controller event event_type=%s direction=%s stage=%s",
		event.SessionID,
		event.Type,
		event.Direction,
		event.Stage,
	)
	switch event.Type {
	case EventSilenceTimeout:
		_ = c.CloseSession(ctx, "silence timeout")
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
		log.Warnf(
			"client_id=%s controller reference queue full queue_len=%d",
			c.deps.SessionID,
			len(c.references),
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

// CloseSession 触发 Session 标准关闭流程。
func (c *Controller) CloseSession(ctx context.Context, reason string) error {
	if c.deps.CloseSession == nil {
		return nil
	}
	log.Infof(
		"client_id=%s controller close session reason=%s",
		c.deps.SessionID,
		reason,
	)
	return c.deps.CloseSession(ctx, reason)
}

// Close 停止 Controller 的后台参考信号分发循环。
func (c *Controller) Close(ctx context.Context) error {
	c.once.Do(func() {
		c.cancel()
		close(c.done)
	})
	return nil
}

// Done 返回 Controller 关闭通知。
func (c *Controller) Done() <-chan struct{} { return c.done }

// InitialSilenceTimeout 返回连接建立后的首次静音等待时长。
func (c *Controller) InitialSilenceTimeout() time.Duration {
	return c.cfg.InitialSilenceTimeout
}

// SilenceTimeout 返回首次有效语音后的静音超时时长。
func (c *Controller) SilenceTimeout() time.Duration {
	return c.cfg.SilenceTimeout
}

// run 异步分发下行参考信号，避免阻塞下行 pipeline。
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

// dispatchReference 把参考帧发送给当前 Session 注册的所有 reference consumer。
func (c *Controller) dispatchReference(ctx context.Context, frame media.Frame) {
	c.mu.RLock()
	consumers := make(map[string]media.ReferenceConsumer, len(c.referenceConsumers))
	for name, consumer := range c.referenceConsumers {
		consumers[name] = consumer
	}
	c.mu.RUnlock()
	for name, consumer := range consumers {
		if err := consumer.AddReference(ctx, frame); err != nil {
			log.Errorf(
				"client_id=%s reference consumer failed consumer=%s error=%v",
				c.deps.SessionID,
				name,
				err,
			)
		}
	}
}

// cloneFrame 深拷贝媒体帧中会被异步访问的可变字段。
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
