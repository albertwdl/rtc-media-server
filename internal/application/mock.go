package application

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
)

const (
	mockResponseAudioDuration = 20 * time.Millisecond
	mockResponseSendGap       = 50 * time.Millisecond
	pcm16BytesPerSample       = 2
	mockTranscriptDelta       = "ok"
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
	audioOutput   connector.AudioOutput
	messageOutput connector.MessageOutput
	cancel        context.CancelFunc
	responseID    uint64
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
func (c *MockConnector) BindAudioOutput(output connector.AudioOutput) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.audioOutput = output
	return nil
}

// BindMessageOutput 绑定服务侧收到后端消息后要推送到的输出端。
func (c *MockConnector) BindMessageOutput(output connector.MessageOutput) error {
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
func (c *MockConnector) SendMessage(ctx context.Context, msg connector.Message) error {
	c.msgCount.Add(1)
	log.Infof(
		"client_id=%s service connector received message protocol=%s direction=%s type=%s bytes=%d",
		msg.SessionID,
		c.Protocol(),
		msg.Direction,
		msg.Type,
		len(msg.Payload),
	)
	switch msg.Type {
	case connector.MessageResponseCreate, connector.MessageInputCommit:
		c.startMockResponse()
	case connector.MessageResponseCancel:
		c.cancelMockResponse()
		_ = c.PushMessage(context.Background(), connector.Message{Type: connector.MessageResponseDone})
	}
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
func (c *MockConnector) PushMessage(ctx context.Context, msg connector.Message) error {
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
		c.cancelMockResponse()
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

// startMockResponse 模拟后端 agent 返回文本、TTS 和完成事件。
func (c *MockConnector) startMockResponse() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.responseID++
	responseID := c.responseID
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.mu.Unlock()

	go func() {
		if err := c.runMockResponse(ctx); err != nil && ctx.Err() == nil {
			log.Errorf("client_id=%s mock service response failed: %v", c.id, err)
		}
		c.clearMockResponse(responseID)
	}()
}

// runMockResponse 按后端返回路径模拟一轮文本和 TTS。
func (c *MockConnector) runMockResponse(ctx context.Context) error {
	if err := waitMockResponseGap(ctx); err != nil {
		return err
	}
	if err := c.PushMessage(ctx, connector.Message{Type: connector.MessageResponseTextDelta, Payload: []byte(mockTranscriptDelta)}); err != nil {
		return err
	}
	if err := waitMockResponseGap(ctx); err != nil {
		return err
	}
	if err := c.PushDownlink(ctx, media.Frame{
		Payload: makeSilentPCM(mockResponseAudioDuration),
		Format:  media.DefaultPCM16Format(),
		Metadata: map[string]string{
			"source": "mock_service",
		},
	}); err != nil {
		return err
	}
	if err := waitMockResponseGap(ctx); err != nil {
		return err
	}
	return c.PushMessage(ctx, connector.Message{Type: connector.MessageResponseDone})
}

// cancelMockResponse 停止当前 mock 后端回复。
func (c *MockConnector) cancelMockResponse() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
}

// clearMockResponse 清理已结束的 mock 回复。
func (c *MockConnector) clearMockResponse(responseID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.responseID == responseID {
		c.cancel = nil
	}
}

// waitMockResponseGap 在 mock 后端事件之间留出短间隔。
func waitMockResponseGap(ctx context.Context) error {
	timer := time.NewTimer(mockResponseSendGap)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// makeSilentPCM 生成用于联调的 PCM16LE 静音帧。
func makeSilentPCM(duration time.Duration) []byte {
	if duration <= 0 {
		duration = mockResponseAudioDuration
	}
	samples := int(duration.Seconds() * float64(media.DefaultAudioSampleRate) * float64(media.DefaultAudioChannels))
	if samples <= 0 {
		samples = 1
	}
	return make([]byte, samples*pcm16BytesPerSample)
}
