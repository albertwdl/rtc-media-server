package vad

import (
	"context"
	"log/slog"
	"sync/atomic"

	"rtc-media-server/internal/media"
)

// MockStage 模拟 VAD 处理模块。
// 当前实现不做真实语音检测，只记录经过该 stage 的媒体帧并透传给后续 stage 或 sink。
type MockStage struct {
	logger *slog.Logger
	count  atomic.Uint64
}

// NewMockStage 创建 VAD mock stage。
func NewMockStage(logger *slog.Logger) *MockStage {
	if logger == nil {
		logger = slog.Default()
	}
	return &MockStage{logger: logger}
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

func (s *MockStage) Close(ctx context.Context) error {
	return nil
}

func (s *MockStage) Count() uint64 {
	return s.count.Load()
}
