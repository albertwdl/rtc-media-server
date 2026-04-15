package media

import (
	"context"
	"time"
)

const (
	KindAudio     = "audio"
	CodecJSON     = "json"
	CodecBase64   = "base64"
	CodecPCM16LE  = "pcm16le"
	CodecG711ALaw = "g711_alaw"
	CodecRTP      = "rtp"
)

const (
	// DefaultAudioSampleRate 是端侧默认 PCM16 采样率。
	DefaultAudioSampleRate = 8000
	// DefaultAudioChannels 是端侧默认声道数。
	DefaultAudioChannels = 1
)

// Direction 表示媒体帧在系统中的方向。
type Direction string

const (
	DirectionUplink   Direction = "uplink"
	DirectionDownlink Direction = "downlink"
)

// Format 描述媒体帧的编码、采样率、声道等格式信息。
// RTP 场景可使用 PayloadType 和 ClockRate 承载 WebRTC/GStreamer 相关参数。
type Format struct {
	Kind        string
	Codec       string
	SampleRate  int
	Channels    int
	PayloadType int
	ClockRate   int
}

// Frame 是 Session、Connector 和 Pipeline 之间传递的统一媒体数据模型。
type Frame struct {
	SessionID string
	Direction Direction
	Seq       uint64
	Timestamp time.Time
	Payload   []byte
	Format    Format
	Metadata  map[string]string
}

// Message 是 Connector 之间透传的非媒体业务消息。
// 消息、控制 JSON 和错误事件不进入媒体 pipeline，而是由 Session 做桥接。
type Message struct {
	SessionID string
	Direction Direction
	Type      string
	Payload   []byte
	Metadata  map[string]string
}

// Event 表示协议适配 stage 上报的非媒体事件。
// Raw 保留原始业务 payload，Fields 用于传递已经解析出的关键字段。
type Event struct {
	Type   string
	Raw    []byte
	Fields map[string]string
}

// StageEvent 表示 stage 抛出的控制事件。
// 事件由 Controller 统一仲裁，stage 不应直接操作 Connector。
type StageEvent struct {
	SessionID string
	Type      string
	Direction Direction
	Stage     string
	FrameSeq  uint64
	Metadata  map[string]string
}

// ReferenceConsumer 接收下行参考信号，典型用途是给 AEC stage 提供回声参考。
type ReferenceConsumer interface {
	AddReference(ctx context.Context, frame Frame) error
}

// Stats 表示 pipeline 的基础运行统计。
type Stats struct {
	Queued    int
	Processed uint64
	Errors    uint64
}

// Input 表示 pipeline 的输入端。
type Input interface {
	Push(ctx context.Context, frame Frame) error
}

// InputFunc 允许用函数快速实现 Input。
type InputFunc func(ctx context.Context, frame Frame) error

// Push 调用函数型 Input 包装的推送函数。
func (fn InputFunc) Push(ctx context.Context, frame Frame) error {
	return fn(ctx, frame)
}

// AudioOutput 表示 Connector 收到外部音频后推送到的输出端。
type AudioOutput interface {
	Push(ctx context.Context, frame Frame) error
}

// Output 表示 pipeline 处理完成后的输出端。
type Output interface {
	SendAudio(ctx context.Context, frame Frame) error
}

// OutputFunc 允许用函数快速实现 Output。
type OutputFunc func(ctx context.Context, frame Frame) error

// SendAudio 调用函数型 Output 包装的发送函数。
func (fn OutputFunc) SendAudio(ctx context.Context, frame Frame) error {
	return fn(ctx, frame)
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

// Pipeline 定义媒体处理管线的最小抽象。
type Pipeline interface {
	Input
	Start(ctx context.Context) error
	Close(ctx context.Context) error
	Stats() Stats
}

// Stage 表示 pipeline 中一个同步处理步骤。
// Stage 必须有稳定名称，方便按配置组装、日志定位和指标统计。
type Stage interface {
	Name() string
	Process(ctx context.Context, frame Frame) (Frame, error)
	Close(ctx context.Context) error
}

// StageFunc 允许用函数快速实现 Stage。
type StageFunc struct {
	name string
	fn   func(ctx context.Context, frame Frame) (Frame, error)
}

// NewStageFunc 创建一个函数型 Stage。
func NewStageFunc(name string, fn func(ctx context.Context, frame Frame) (Frame, error)) StageFunc {
	return StageFunc{name: name, fn: fn}
}

// Name 返回函数型 stage 的名称。
func (fn StageFunc) Name() string {
	if fn.name == "" {
		return "stage_func"
	}
	return fn.name
}

// Process 执行函数型 stage 包装的处理函数。
func (fn StageFunc) Process(ctx context.Context, frame Frame) (Frame, error) {
	if fn.fn == nil {
		return frame, nil
	}
	return fn.fn(ctx, frame)
}

// Close 关闭函数型 stage。
func (fn StageFunc) Close(ctx context.Context) error {
	return nil
}

// DefaultPCM16Format 返回当前端侧协议使用的 PCM16 8k 单声道格式。
func DefaultPCM16Format() Format {
	return Format{
		Kind:       KindAudio,
		Codec:      CodecPCM16LE,
		SampleRate: DefaultAudioSampleRate,
		Channels:   DefaultAudioChannels,
	}
}
