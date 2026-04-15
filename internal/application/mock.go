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
	id       string
	done     chan struct{}
	once     sync.Once
	count    atomic.Uint64
	msgCount atomic.Uint64

	mu            sync.RWMutex
	audioOutput   media.AudioOutput
	messageOutput media.MessageOutput
}

// NewMockConnector 创建服务侧 mock connector。
func NewMockConnector(id string) *MockConnector {
	return &MockConnector{id: id, done: make(chan struct{})}
}

// ID 返回 mock 服务连接器绑定的 Session ID。
func (c *MockConnector) ID() string { return c.id }

// Protocol 返回 mock 服务连接器协议名称。
func (c *MockConnector) Protocol() string { return "mock_service" }

// BindAudioOutput 绑定服务侧收到后端音频后要推送到的输出端。
func (c *MockConnector) BindAudioOutput(output media.AudioOutput) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.audioOutput = output
	return nil
}

// BindMessageOutput 绑定服务侧收到后端消息后要推送到的输出端。
func (c *MockConnector) BindMessageOutput(output media.MessageOutput) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messageOutput = output
	return nil
}

// SendAudio 接收经过完整上行 pipeline 处理后的音频帧。
func (c *MockConnector) SendAudio(ctx context.Context, frame media.Frame) error {
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

// SendMessage 接收经过 Session 桥接后的消息。
func (c *MockConnector) SendMessage(ctx context.Context, msg media.Message) error {
	c.msgCount.Add(1)
	log.Infof(
		"client_id=%s service connector received message protocol=%s direction=%s type=%s bytes=%d",
		msg.SessionID,
		c.Protocol(),
		msg.Direction,
		msg.Type,
		len(msg.Payload),
	)
	return nil
}

// PushDownlink 用于测试或 demo 将服务侧 PCM 投递回当前 Session。
func (c *MockConnector) PushDownlink(ctx context.Context, frame media.Frame) error {
	c.mu.RLock()
	output := c.audioOutput
	c.mu.RUnlock()
	if output == nil {
		return nil
	}
	return output.Push(ctx, frame)
}

// PushMessage 用于测试或 demo 将服务侧非媒体消息投递回当前 Session。
func (c *MockConnector) PushMessage(ctx context.Context, msg media.Message) error {
	c.mu.RLock()
	output := c.messageOutput
	c.mu.RUnlock()
	if output == nil {
		return nil
	}
	return output.PushMessage(ctx, msg)
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

// MessageCount 返回 mock 服务连接器已消费的非媒体消息数量。
func (c *MockConnector) MessageCount() uint64 { return c.msgCount.Load() }
