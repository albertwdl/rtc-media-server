package application

import (
	"context"
	"sync"
	"sync/atomic"

	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
)

// MockConnector 模拟服务侧连接器。
// 当前 demo 只记录上行媒体帧，真实业务接入时替换为实际 ServiceConnector。
type MockConnector struct {
	id    string
	done  chan struct{}
	once  sync.Once
	count atomic.Uint64

	mu        sync.RWMutex
	dataInput media.Input
}

// NewMockConnector 创建服务侧 mock connector。
func NewMockConnector(id string) *MockConnector {
	return &MockConnector{id: id, done: make(chan struct{})}
}

// ID 返回 mock 服务连接器绑定的 Session ID。
func (c *MockConnector) ID() string { return c.id }

// Protocol 返回 mock 服务连接器协议名称。
func (c *MockConnector) Protocol() string { return "mock_service" }

// BindInput 绑定服务侧收到下行数据后要推送到的 pipeline 输入端。
func (c *MockConnector) BindInput(input media.Input) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dataInput = input
	return nil
}

// SendData 接收经过完整上行 pipeline 处理后的媒体帧。
func (c *MockConnector) SendData(ctx context.Context, frame media.Frame) error {
	c.count.Add(1)
	log.Infof(
		"client_id=%s service connector received frame protocol=%s direction=%s codec=%s bytes=%d",
		frame.SessionID,
		c.Protocol(),
		frame.Direction,
		frame.Format.Codec,
		len(frame.Payload),
	)
	return nil
}

// PushDownlink 用于测试或 demo 将服务侧 PCM 投递回当前 Session。
func (c *MockConnector) PushDownlink(ctx context.Context, frame media.Frame) error {
	c.mu.RLock()
	input := c.dataInput
	c.mu.RUnlock()
	if input == nil {
		return nil
	}
	return input.Push(ctx, frame)
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
