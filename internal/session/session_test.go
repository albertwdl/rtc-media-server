package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"rtc-media-server/internal/media"
)

// TestNewSessionCreatesFixedComponents 验证 NewSession 会创建并启动固定链路组件。
func TestNewSessionCreatesFixedComponents(t *testing.T) {
	client := &testClientConnector{id: "client-a", done: make(chan struct{})}
	sess, err := NewSession(context.Background(), testConfig(), client)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background(), "test done")

	if client.input == nil {
		t.Fatal("client BindInput did not bind uplink input")
	}
	if sess.ServiceConnector() == nil {
		t.Fatal("service connector was not created")
	}
	if sess.Controller() == nil {
		t.Fatal("controller was not created")
	}
	if sess.Uplink() == nil || sess.Downlink() == nil {
		t.Fatal("pipelines were not created")
	}
}

// TestSessionDownlinkPipelineConsumesToClient 验证下行 PCM 会通过固定 pipeline 投递给客户端 Connector。
func TestSessionDownlinkPipelineConsumesToClient(t *testing.T) {
	client := &testClientConnector{
		id:       "client-b",
		done:     make(chan struct{}),
		consumed: make(chan media.Frame, 1),
	}
	sess, err := NewSession(context.Background(), testConfig(), client)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background(), "test done")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sess.EnqueueDownlink(ctx, media.Frame{
		Payload: []byte{0x00, 0x00, 0x00, 0x01},
		Format:  media.DefaultPCM16Format(),
	}); err != nil {
		t.Fatalf("EnqueueDownlink: %v", err)
	}

	select {
	case frame := <-client.consumed:
		if frame.Format.Codec != media.CodecBase64 {
			t.Fatalf("downlink codec = %q", frame.Format.Codec)
		}
		if frame.Direction != media.DirectionDownlink {
			t.Fatalf("downlink direction = %q", frame.Direction)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for client consume")
	}
}

// TestSessionMessageBridge 验证非媒体消息只通过 Session 桥接，不进入媒体 pipeline。
func TestSessionMessageBridge(t *testing.T) {
	client := &testClientConnector{
		id:       "client-msg",
		done:     make(chan struct{}),
		messages: make(chan media.Message, 1),
	}
	sess, err := NewSession(context.Background(), testConfig(), client)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background(), "test done")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if client.msgInput == nil {
		t.Fatal("client BindMessageInput did not bind message input")
	}
	if err := client.msgInput.PushMessage(ctx, media.Message{
		Type:    "response.create",
		Payload: []byte(`{"type":"response.create"}`),
	}); err != nil {
		t.Fatalf("push client message: %v", err)
	}
	waitForServiceMessageCount(t, ctx, sess, 1)

	service, ok := sess.ServiceConnector().(interface {
		PushMessage(context.Context, media.Message) error
	})
	if !ok {
		t.Fatalf("service connector %T does not expose PushMessage", sess.ServiceConnector())
	}
	if err := service.PushMessage(ctx, media.Message{
		Type:    "response.text.delta",
		Payload: []byte(`{"type":"response.text.delta","text":"ok"}`),
	}); err != nil {
		t.Fatalf("push service message: %v", err)
	}

	select {
	case msg := <-client.messages:
		if msg.Type != "response.text.delta" {
			t.Fatalf("message type = %q", msg.Type)
		}
		if msg.Direction != media.DirectionDownlink {
			t.Fatalf("message direction = %q", msg.Direction)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for client message")
	}
}

// TestManagerAttachReusesExistingSession 验证 Manager 只为同一客户端创建一次 Session。
func TestManagerAttachReusesExistingSession(t *testing.T) {
	manager := NewManager(testConfig())
	client := &testClientConnector{id: "client-c", done: make(chan struct{})}

	first, createdFirst, err := manager.Attach(context.Background(), client)
	if err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	defer first.Close(context.Background(), "test done")

	second, createdSecond, err := manager.Attach(context.Background(), client)
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}
	if first != second || !createdFirst || createdSecond {
		t.Fatalf("unexpected attach result first=%p second=%p createdFirst=%v createdSecond=%v", first, second, createdFirst, createdSecond)
	}
}

// TestManagerRemoveClosesSession 验证 Remove 会关闭并移除 Session。
func TestManagerRemoveClosesSession(t *testing.T) {
	client := &testClientConnector{id: "client-d", done: make(chan struct{})}
	manager := NewManager(testConfig())
	sess, _, err := manager.Attach(context.Background(), client)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	removeErr := context.Canceled
	manager.Remove(context.Background(), client.ID(), removeErr)
	if _, ok := manager.Get(client.ID()); ok {
		t.Fatal("session still exists after Remove")
	}
	if sess.Err() != removeErr {
		t.Fatalf("Err = %v", sess.Err())
	}
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Fatal("session was not closed")
	}
}

// TestManagerCloseClosesAllSessions 验证 Manager.Close 会关闭所有已管理 Session。
func TestManagerCloseClosesAllSessions(t *testing.T) {
	manager := NewManager(testConfig())
	first, _, err := manager.Attach(context.Background(), &testClientConnector{id: "client-e", done: make(chan struct{})})
	if err != nil {
		t.Fatalf("attach first: %v", err)
	}
	second, _, err := manager.Attach(context.Background(), &testClientConnector{id: "client-f", done: make(chan struct{})})
	if err != nil {
		t.Fatalf("attach second: %v", err)
	}

	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, sess := range []*Session{first, second} {
		select {
		case <-sess.Done():
		case <-time.After(time.Second):
			t.Fatalf("session %s was not closed", sess.ID())
		}
	}
}

// testConfig 返回 Session 单测使用的最小运行配置。
func testConfig() Config {
	return Config{
		UplinkQueueSize:   8,
		DownlinkQueueSize: 8,
		TargetFormat:      media.DefaultPCM16Format(),
	}
}

// testClientConnector 是 Session 单测使用的客户端 Connector。
type testClientConnector struct {
	id       string
	done     chan struct{}
	input    media.Input
	msgInput media.MessageInput
	consumed chan media.Frame
	messages chan media.Message
	closed   bool
	mu       sync.Mutex
}

// ID 返回测试客户端 ID。
func (c *testClientConnector) ID() string { return c.id }

// Protocol 返回测试客户端协议名称。
func (c *testClientConnector) Protocol() string { return "test_client" }

// BindInput 绑定测试客户端收到数据后要推送到的 pipeline 输入端。
func (c *testClientConnector) BindInput(input media.Input) error {
	c.input = input
	return nil
}

// BindMessageInput 绑定测试客户端收到非媒体消息后要推送到的输入端。
func (c *testClientConnector) BindMessageInput(input media.MessageInput) error {
	c.msgInput = input
	return nil
}

// SendData 记录测试下行媒体帧。
func (c *testClientConnector) SendData(ctx context.Context, frame media.Frame) error {
	if c.consumed != nil {
		select {
		case c.consumed <- frame:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// SendMessage 记录测试下行非媒体消息。
func (c *testClientConnector) SendMessage(ctx context.Context, msg media.Message) error {
	if c.messages != nil {
		select {
		case c.messages <- msg:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// MeasureRTT 返回固定 RTT 供测试使用。
func (c *testClientConnector) MeasureRTT(ctx context.Context) (time.Duration, error) {
	return time.Millisecond, nil
}

// Close 关闭测试客户端 Connector。
func (c *testClientConnector) Close(ctx context.Context, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && c.done != nil {
		c.closed = true
		close(c.done)
	}
	return nil
}

// Done 返回测试客户端关闭通知。
func (c *testClientConnector) Done() <-chan struct{} { return c.done }

// waitForServiceMessageCount 等待 mock service connector 消费到指定非媒体消息数。
func waitForServiceMessageCount(t *testing.T, ctx context.Context, sess *Session, want uint64) {
	t.Helper()
	counter, ok := sess.ServiceConnector().(interface{ MessageCount() uint64 })
	if !ok {
		t.Fatalf("service connector %T does not expose MessageCount", sess.ServiceConnector())
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := counter.MessageCount(); got >= want {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for service message count %d, got %d", want, counter.MessageCount())
		}
	}
}
