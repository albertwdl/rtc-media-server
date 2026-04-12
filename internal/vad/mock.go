package vad

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"rtc-media-server/internal/controller"
	"rtc-media-server/internal/media"
)

// MockStage 模拟 VAD 处理模块。
// 当前实现不做真实语音检测，只记录经过该 stage 的媒体帧并透传给后续 stage 或 sink。
type MockStage struct {
	logger              *slog.Logger
	emit                func(ctx context.Context, event media.StageEvent)
	initialSilenceLimit time.Duration
	silenceLimit        time.Duration
	count               atomic.Uint64
}

// NewMockStage 创建 VAD mock stage。
func NewMockStage(logger *slog.Logger) *MockStage {
	if logger == nil {
		logger = slog.Default()
	}
	return &MockStage{
		logger:              logger,
		initialSilenceLimit: 15 * time.Second,
		silenceLimit:        5 * time.Second,
	}
}

// SetEventEmitter 设置 VAD 事件上报函数。
func (s *MockStage) SetEventEmitter(emit func(ctx context.Context, event media.StageEvent)) {
	s.emit = emit
}

func (s *MockStage) Name() string { return "vad_mock" }

// Process 接收上行增强后的 PCM 帧，记录 VAD mock 日志并透传。
func (s *MockStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	s.count.Add(1)
	s.logger.Info(
		"client_id="+frame.SessionID+" vad processed pcm",
		slog.String("client_id", frame.SessionID),
		slog.String("stage", s.Name()),
		slog.String("direction", string(frame.Direction)),
		slog.String("codec", frame.Format.Codec),
		slog.Int("bytes", len(frame.Payload)),
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

func (s *MockStage) Close(ctx context.Context) error {
	return nil
}

func (s *MockStage) Count() uint64 {
	return s.count.Load()
}
