package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"rtc-media-server/internal/media"
)

// TestAttachRequiresServiceConnector 验证缺少服务侧 Connector 工厂时拒绝创建 Session。
func TestAttachRequiresServiceConnector(t *testing.T) {
	manager := NewManager(Config{}, Dependencies{})
	client := &testClientConnector{id: "client-a", done: make(chan struct{})}

	_, _, err := manager.Attach(context.Background(), client)
	if err == nil {
		t.Fatal("Attach returned nil error")
	}
	if !strings.Contains(err.Error(), "NewServiceConnector") {
		t.Fatalf("error = %v", err)
	}
}

// testClientConnector 是 Session 单测使用的客户端 Connector。
type testClientConnector struct {
	id   string
	done chan struct{}
}

// ID 返回测试客户端 ID。
func (c *testClientConnector) ID() string { return c.id }

// Protocol 返回测试客户端协议名称。
func (c *testClientConnector) Protocol() string { return "test_client" }

// Start 启动测试客户端 Connector。
func (c *testClientConnector) Start(ctx context.Context, sink media.Sink) error { return nil }

// Consume 消费测试下行媒体帧。
func (c *testClientConnector) Consume(ctx context.Context, frame media.Frame) error { return nil }

// MeasureRTT 返回固定 RTT 供测试使用。
func (c *testClientConnector) MeasureRTT(ctx context.Context) (time.Duration, error) {
	return time.Millisecond, nil
}

// Close 关闭测试客户端 Connector。
func (c *testClientConnector) Close(ctx context.Context, reason string) error { return nil }

// Done 返回测试客户端关闭通知。
func (c *testClientConnector) Done() <-chan struct{} { return c.done }
