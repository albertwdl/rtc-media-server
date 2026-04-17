package connector

import (
	"context"
	"time"

	"rtc-media-server/internal/media"
)

const (
	// MessageSessionCreated 表示会话已创建。
	MessageSessionCreated = "session_created"
	// MessageSessionUpdated 表示端侧完成会话配置。
	MessageSessionUpdated = "session_updated"
	// MessageInputCommit 表示端侧显式提交当前输入音频。
	MessageInputCommit = "input_commit"
	// MessageResponseCreate 表示请求服务侧生成回复。
	MessageResponseCreate = "response_create"
	// MessageResponseCancel 表示请求服务侧取消当前回复。
	MessageResponseCancel = "response_cancel"
	// MessageSpeechStarted 表示内部检测到上行语音开始。
	MessageSpeechStarted = "speech_started"
	// MessageSpeechStopped 表示内部检测到上行语音结束。
	MessageSpeechStopped = "speech_stopped"
	// MessageResponseTextDelta 表示服务侧返回一段回复文本。
	MessageResponseTextDelta = "response_text_delta"
	// MessageResponseDone 表示服务侧当前回复结束。
	MessageResponseDone = "response_done"
	// MessageError 表示需要向连接另一端发送错误消息。
	MessageError = "error"
)

// Message 是 Connector 之间透传的非媒体业务消息。
// 消息、控制 JSON 和错误事件不进入媒体 pipeline，而是由 Session 做桥接。
type Message struct {
	SessionID string
	Direction media.Direction
	Type      string
	Payload   []byte
	Metadata  map[string]string
}

// AudioOutput 表示 Connector 收到外部音频后推送到的输出端。
type AudioOutput interface {
	Push(ctx context.Context, frame media.Frame) error
}

// MessageOutput 表示 Connector 收到外部消息后推送到的输出端。
type MessageOutput interface {
	PushMessage(ctx context.Context, msg Message) error
}

// MessageOutputFunc 允许用函数快速实现 MessageOutput。
type MessageOutputFunc func(ctx context.Context, msg Message) error

// PushMessage 调用函数型 MessageOutput 包装的推送函数。
func (fn MessageOutputFunc) PushMessage(ctx context.Context, msg Message) error {
	return fn(ctx, msg)
}

// ClientConnector 表示某个客户端的一条双向连接能力。
// 它不是全局监听器，而是 Session 持有的客户端连接抽象。
type ClientConnector interface {
	ID() string
	Protocol() string

	// BindAudioOutput 绑定客户端连接收到外部音频后要推送到的输出端。
	BindAudioOutput(output AudioOutput) error
	// BindMessageOutput 绑定客户端连接收到外部消息后要推送到的输出端。
	BindMessageOutput(output MessageOutput) error
	// SendAudio 向客户端连接发送音频帧。
	SendAudio(ctx context.Context, frame media.Frame) error
	// SendMessage 向客户端连接发送消息。
	SendMessage(ctx context.Context, msg Message) error

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
	BindAudioOutput(output AudioOutput) error
	// BindMessageOutput 绑定服务侧连接收到外部消息后要推送到的输出端。
	BindMessageOutput(output MessageOutput) error
	// SendAudio 向服务侧连接发送音频帧。
	SendAudio(ctx context.Context, frame media.Frame) error
	// SendMessage 向服务侧连接发送消息。
	SendMessage(ctx context.Context, msg Message) error
	Close(ctx context.Context, reason string) error
	Done() <-chan struct{}
}
