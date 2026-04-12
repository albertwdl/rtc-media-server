package vad

import (
	"context"
	"sync/atomic"
	"time"

	"rtc-media-server/internal/controller"
	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
)

// MockStage 模拟 VAD 处理模块。
// 当前实现不做真实语音检测，只记录经过该 stage 的媒体帧并透传给后续 stage 或 sink。
type MockStage struct {
	emit                func(ctx context.Context, event media.StageEvent)
	initialSilenceLimit time.Duration
	silenceLimit        time.Duration
	count               atomic.Uint64
}

// NewMockStage 创建 VAD mock stage。
func NewMockStage() *MockStage {
	return NewMockStageWithTimeouts(15*time.Second, 5*time.Second)
}

// NewMockStageWithTimeouts 创建带静音超时配置的 VAD mock stage。
func NewMockStageWithTimeouts(initialSilence, silence time.Duration) *MockStage {
	if initialSilence <= 0 {
		initialSilence = 15 * time.Second
	}
	if silence <= 0 {
		silence = 5 * time.Second
	}
	return &MockStage{
		initialSilenceLimit: initialSilence,
		silenceLimit:        silence,
	}
}

// SetEventEmitter 设置 VAD 事件上报函数。
func (s *MockStage) SetEventEmitter(emit func(ctx context.Context, event media.StageEvent)) {
	s.emit = emit
}

// Name 返回 VAD mock stage 名称。
func (s *MockStage) Name() string { return "vad_mock" }

// Process 接收上行增强后的 PCM 帧，记录 VAD mock 日志并透传。
func (s *MockStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	s.count.Add(1)
	log.Infof(
		"client_id=%s vad processed pcm stage=%s direction=%s codec=%s bytes=%d",
		frame.SessionID,
		s.Name(),
		frame.Direction,
		frame.Format.Codec,
		len(frame.Payload),
	)
	return frame, nil
}

// EmitSilenceTimeout 供测试或后续真实 VAD 定时器触发静音超时事件。
func (s *MockStage) EmitSilenceTimeout(ctx context.Context, frame media.Frame) {
	if s.emit == nil {
		return
	}
	s.emit(ctx, media.StageEvent{
		SessionID: frame.SessionID,
		Type:      controller.EventSilenceTimeout,
		Direction: frame.Direction,
		Stage:     s.Name(),
		FrameSeq:  frame.Seq,
		Metadata: map[string]string{
			"initial_silence_timeout": s.initialSilenceLimit.String(),
			"silence_timeout":         s.silenceLimit.String(),
		},
	})
}

// Close 关闭 VAD mock stage。
func (s *MockStage) Close(ctx context.Context) error {
	return nil
}

// Count 返回 VAD mock stage 已处理的帧数量。
func (s *MockStage) Count() uint64 {
	return s.count.Load()
}
