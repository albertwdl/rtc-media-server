package connector

import (
	"context"
	"time"

	"rtc-media-server/internal/media"
)

// ClientConnector 表示某个客户端的一条双向连接能力。
// 它不是全局监听器，而是 Session 持有的客户端连接抽象。
type ClientConnector interface {
	ID() string
	Protocol() string

	BindInput(input media.Input) error
	SendData(ctx context.Context, frame media.Frame) error

	MeasureRTT(ctx context.Context) (time.Duration, error)
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}

// ServiceConnector 表示业务服务侧连接。
// 上行通过 SendData 接收处理后的媒体帧；下行通过 BindInput 绑定的输入端投递返回媒体帧。
type ServiceConnector interface {
	ID() string
	Protocol() string

	BindInput(input media.Input) error
	SendData(ctx context.Context, frame media.Frame) error
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}
