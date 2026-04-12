package application

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"rtc-media-server/internal/media"
)

// MockConnector 模拟服务侧连接器。
// 当前 demo 只记录上行媒体帧，真实业务接入时替换为实际 ServiceConnector。
type MockConnector struct {
	id     string
	logger *slog.Logger
	done   chan struct{}
	once   sync.Once
	count  atomic.Uint64

	mu       sync.RWMutex
	downlink media.Sink
}

// NewMockConnector 创建服务侧 mock connector。
func NewMockConnector(id string, logger *slog.Logger) *MockConnector {
	if logger == nil {
		logger = slog.Default()
	}
	return &MockConnector{id: id, logger: logger, done: make(chan struct{})}
}

// ID 返回 mock 服务连接器绑定的 Session ID。
func (c *MockConnector) ID() string { return c.id }

// Protocol 返回 mock 服务连接器协议名称。
func (c *MockConnector) Protocol() string { return "mock_service" }

// Start 绑定服务侧下行输出目标。
func (c *MockConnector) Start(ctx context.Context, downlink media.Sink) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.downlink = downlink
	return nil
}

// Consume 接收经过完整上行 pipeline 处理后的媒体帧。
func (c *MockConnector) Consume(ctx context.Context, frame media.Frame) error {
	c.count.Add(1)
	c.logger.Info(
		"client_id="+frame.SessionID+" service connector received frame",
		slog.String("client_id", frame.SessionID),
		slog.String("protocol", c.Protocol()),
		slog.String("direction", string(frame.Direction)),
		slog.String("codec", frame.Format.Codec),
		slog.Int("bytes", len(frame.Payload)),
	)
	return nil
}

// PushDownlink 用于测试或 demo 将服务侧 PCM 投递回当前 Session。
func (c *MockConnector) PushDownlink(ctx context.Context, frame media.Frame) error {
	c.mu.RLock()
	downlink := c.downlink
	c.mu.RUnlock()
	if downlink == nil {
		return nil
	}
	return downlink.Consume(ctx, frame)
}

// Close 关闭 mock 服务连接器。
func (c *MockConnector) Close(ctx context.Context, reason string) error {
	c.once.Do(func() {
		close(c.done)
	})
	return nil
}

// Done 返回 mock 服务连接器关闭通知。
func (c *MockConnector) Done() <-chan struct{} { return c.done }

// Count 返回 mock 服务连接器已消费的上行帧数量。
func (c *MockConnector) Count() uint64 { return c.count.Load() }
