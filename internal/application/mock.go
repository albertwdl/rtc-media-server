package application

import (
	"context"
	"log/slog"
	"sync/atomic"

	"rtc-media-server/internal/media"
)

// MockSink 模拟上行 pipeline 最后的业务消费端。
// 当前 demo 只记录收到的媒体帧，真实业务接入时替换为实际 sink 即可。
type MockSink struct {
	logger *slog.Logger
	count  atomic.Uint64
}

// NewMockSink 创建业务消费端 mock。
func NewMockSink(logger *slog.Logger) *MockSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &MockSink{logger: logger}
}

// Consume 接收经过完整上行 pipeline 处理后的媒体帧。
func (s *MockSink) Consume(ctx context.Context, frame media.Frame) error {
	s.count.Add(1)
	s.logger.Info(
		"client_id="+frame.SessionID+" application sink received frame",
		slog.String("client_id", frame.SessionID),
		slog.String("direction", string(frame.Direction)),
		slog.String("codec", frame.Format.Codec),
		slog.Int("bytes", len(frame.Payload)),
	)
	return nil
}

func (s *MockSink) Count() uint64 {
	return s.count.Load()
}
