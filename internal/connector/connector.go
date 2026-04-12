package connector

import (
	"context"
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
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}
