package endpoint

import (
	"context"
	"time"

	"rtc-media-server/internal/media"
)

// Event 表示 Endpoint 上报的非媒体事件。
// Raw 保留协议层收到的原始业务 payload，Fields 用于传递已经解析出的关键字段。
type Event struct {
	Type   string
	Raw    []byte
	Fields map[string]string
}

// Endpoint 表示某个客户端的一条双向连接能力。
// 它不是全局监听器，而是 Session 持有的客户端连接抽象。
type Endpoint interface {
	ID() string
	Protocol() string

	media.Source
	media.Sink

	SendCommand(ctx context.Context, payload []byte) error
	MeasureRTT(ctx context.Context) (time.Duration, error)
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}
