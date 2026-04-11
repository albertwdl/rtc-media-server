package media

import (
	"context"
	"time"
)

const (
	KindAudio     = "audio"
	CodecPCM16LE  = "pcm16le"
	CodecG711ALaw = "g711_alaw"
	CodecRTP      = "rtp"
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

// Frame 是 Session、Transport 和 Pipeline 之间传递的统一媒体数据模型。
type Frame struct {
	SessionID string
	Direction Direction
	Seq       uint64
	Timestamp time.Time
	Payload   []byte
	Format    Format
	Metadata  map[string]string
}

// Stats 表示 pipeline 的基础运行统计。
type Stats struct {
	Queued    int
	Processed uint64
	Errors    uint64
}

// Source 表示 pipeline 的输入端。
// WebSocket、WebRTC 等客户端连接都可以作为上行 source。
type Source interface {
	Start(ctx context.Context, sink Sink) error
}

// Pipeline 定义媒体处理管线的最小抽象。
type Pipeline interface {
	Start(ctx context.Context) error
	Push(ctx context.Context, frame Frame) error
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

func (fn StageFunc) Name() string {
	if fn.name == "" {
		return "stage_func"
	}
	return fn.name
}

func (fn StageFunc) Process(ctx context.Context, frame Frame) (Frame, error) {
	if fn.fn == nil {
		return frame, nil
	}
	return fn.fn(ctx, frame)
}

func (fn StageFunc) Close(ctx context.Context) error {
	return nil
}

// Sink 表示 pipeline 的终点。
type Sink interface {
	Consume(ctx context.Context, frame Frame) error
}

// SinkFunc 允许用函数快速实现 Sink。
type SinkFunc func(ctx context.Context, frame Frame) error

func (fn SinkFunc) Consume(ctx context.Context, frame Frame) error {
	return fn(ctx, frame)
}

// DefaultPCM16Format 返回当前端侧协议使用的 PCM16 8k 单声道格式。
func DefaultPCM16Format() Format {
	return Format{
		Kind:       KindAudio,
		Codec:      CodecPCM16LE,
		SampleRate: 8000,
		Channels:   1,
	}
}
