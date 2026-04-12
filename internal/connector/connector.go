package connector

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"rtc-media-server/internal/media"
)

// Event 表示 Connector 或协议适配 stage 上报的非媒体事件。
// Raw 保留原始业务 payload，Fields 用于传递已经解析出的关键字段。
type Event struct {
	Type   string
	Raw    []byte
	Fields map[string]string
}

// ClientConnector 表示某个客户端的一条双向连接能力。
// 它不是全局监听器，而是 Session 持有的客户端连接抽象。
type ClientConnector interface {
	ID() string
	Protocol() string

	media.Source
	media.Sink

	SendCommand(ctx context.Context, payload []byte) error
	MeasureRTT(ctx context.Context) (time.Duration, error)
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}

// ServiceConnector 表示业务服务侧连接。
// 上行作为 Sink 消费处理后的媒体帧；下行通过 Start 绑定的 sink 投递返回媒体帧。
type ServiceConnector interface {
	ID() string
	Protocol() string

	media.Sink

	Start(ctx context.Context, downlink media.Sink) error
	Flush(ctx context.Context, reason string) error
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}

// NoopServiceConnector 是默认空服务连接器，用于未接入业务服务时保持管线可运行。
type NoopServiceConnector struct {
	id     string
	done   chan struct{}
	once   sync.Once
	count  atomic.Uint64
	closed atomic.Bool
}

func NewNoopServiceConnector(id string) *NoopServiceConnector {
	return &NoopServiceConnector{id: id, done: make(chan struct{})}
}

func (c *NoopServiceConnector) ID() string { return c.id }

func (c *NoopServiceConnector) Protocol() string { return "noop_service" }

func (c *NoopServiceConnector) Start(ctx context.Context, downlink media.Sink) error { return nil }

func (c *NoopServiceConnector) Consume(ctx context.Context, frame media.Frame) error {
	c.count.Add(1)
	return nil
}

func (c *NoopServiceConnector) Flush(ctx context.Context, reason string) error { return nil }

func (c *NoopServiceConnector) Close(ctx context.Context, reason string) error {
	c.once.Do(func() {
		c.closed.Store(true)
		close(c.done)
	})
	return nil
}

func (c *NoopServiceConnector) Done() <-chan struct{} { return c.done }

func (c *NoopServiceConnector) Count() uint64 { return c.count.Load() }
