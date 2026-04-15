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

	// BindAudioOutput 绑定客户端连接收到外部音频后要推送到的输出端。
	BindAudioOutput(output media.AudioOutput) error
	// BindMessageOutput 绑定客户端连接收到外部消息后要推送到的输出端。
	BindMessageOutput(output media.MessageOutput) error
	// SendAudio 向客户端连接发送音频帧。
	SendAudio(ctx context.Context, frame media.Frame) error
	// SendMessage 向客户端连接发送消息。
	SendMessage(ctx context.Context, msg media.Message) error

	MeasureRTT(ctx context.Context) (time.Duration, error)
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}

// ServiceConnector 表示业务服务侧连接。
// 上行通过 SendAudio 接收处理后的音频帧；下行通过绑定的输出端投递返回数据。
type ServiceConnector interface {
	ID() string
	Protocol() string

	// BindAudioOutput 绑定服务侧连接收到外部音频后要推送到的输出端。
	BindAudioOutput(output media.AudioOutput) error
	// BindMessageOutput 绑定服务侧连接收到外部消息后要推送到的输出端。
	BindMessageOutput(output media.MessageOutput) error
	// SendAudio 向服务侧连接发送音频帧。
	SendAudio(ctx context.Context, frame media.Frame) error
	// SendMessage 向服务侧连接发送消息。
	SendMessage(ctx context.Context, msg media.Message) error
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}
