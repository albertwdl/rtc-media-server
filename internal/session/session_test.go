package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"rtc-media-server/internal/config"
	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/controller"
	"rtc-media-server/internal/media"
)

// TestConfigFromAppConfig 验证应用配置会转换为 Session 运行配置。
func TestConfigFromAppConfig(t *testing.T) {
	appCfg := config.DefaultConfig()
	appCfg.Session.CloseTimeout = config.Duration(4 * time.Second)
	appCfg.Controller.InitialSilenceTimeout = config.Duration(11 * time.Second)
	appCfg.Controller.SilenceTimeout = config.Duration(6 * time.Second)
	appCfg.Controller.ReferenceQueueSize = 9

	cfg := ConfigFromAppConfig(appCfg)
	if cfg.CloseTimeout != 4*time.Second {
		t.Fatalf("CloseTimeout = %s", cfg.CloseTimeout)
	}
	if cfg.Controller.InitialSilenceTimeout != 11*time.Second {
		t.Fatalf("InitialSilenceTimeout = %s", cfg.Controller.InitialSilenceTimeout)
	}
	if cfg.Controller.SilenceTimeout != 6*time.Second {
		t.Fatalf("SilenceTimeout = %s", cfg.Controller.SilenceTimeout)
	}
	if cfg.Controller.ReferenceQueueSize != 9 {
		t.Fatalf("ReferenceQueueSize = %d", cfg.Controller.ReferenceQueueSize)
	}
	if cfg.UplinkQueueSize != 0 || cfg.DownlinkQueueSize != 0 || cfg.TargetFormat.Kind != "" {
		t.Fatalf("pipeline defaults should remain normalized by session: %+v", cfg)
	}
}

// TestNewSessionCreatesFixedComponents 验证 NewSession 会创建并启动固定链路组件。
func TestNewSessionCreatesFixedComponents(t *testing.T) {
	client := &testClientConnector{id: "client-a", done: make(chan struct{})}
	sess, err := NewSession(context.Background(), testConfig(), client)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background(), "test done")

	if client.audioOutput == nil {
		t.Fatal("client BindAudioOutput did not bind audio output")
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

// TestSessionUpdateRTTFeedsAudioEnhancement 验证 3A stage 可通过 Session getter 读取 RTT。
func TestSessionUpdateRTTFeedsAudioEnhancement(t *testing.T) {
	client := &testClientConnector{
		id:       "client-rtt-downlink",
		done:     make(chan struct{}),
		consumed: make(chan media.Frame, 1),
	}
	sess, err := NewSession(context.Background(), testConfig(), client)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background(), "test done")
	sess.UpdateRTT(25 * time.Millisecond)

	rtt, ok := sess.RTT()
	if !ok {
		t.Fatal("rtt was not cached")
	}
	if rtt != 25*time.Millisecond {
		t.Fatalf("rtt = %s", rtt)
	}
}

// TestSessionServiceMessageBridge 验证服务侧消息会通过 Session 桥接到端侧。
func TestSessionServiceMessageBridge(t *testing.T) {
	client := &testClientConnector{
		id:       "client-msg",
		done:     make(chan struct{}),
		messages: make(chan connector.Message, 2),
	}
	sess, err := NewSession(context.Background(), testConfig(), client)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background(), "test done")

	if client.messageOutput == nil {
		t.Fatal("client BindMessageOutput did not bind message output")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	service, ok := sess.ServiceConnector().(interface {
		PushMessage(context.Context, connector.Message) error
	})
	if !ok {
		t.Fatalf("service connector %T does not expose PushMessage", sess.ServiceConnector())
	}
	if err := service.PushMessage(ctx, connector.Message{
		Type:    "response.text.delta",
		Payload: []byte(`{"type":"response.text.delta","text":"ok"}`),
	}); err != nil {
		t.Fatalf("push service message: %v", err)
	}

	expectClientMessageType(t, ctx, client.messages, "response.text.delta")
}

// TestSessionClientMessageBridge 验证端侧消息会通过 Session 桥接到服务侧。
func TestSessionClientMessageBridge(t *testing.T) {
	client := &testClientConnector{
		id:   "client-command",
		done: make(chan struct{}),
	}
	sess, err := NewSession(context.Background(), testConfig(), client)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background(), "test done")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := sess.OnClientMessage(ctx, connector.Message{Type: connector.MessageResponseCreate}); err != nil {
		t.Fatalf("client message: %v", err)
	}
	waitForServiceMessageCount(t, ctx, sess, 1)
}

// TestSessionStageSpeechEventsForwardToClient 验证 VAD 语音起止事件会转成中性下行消息。
func TestSessionStageSpeechEventsForwardToClient(t *testing.T) {
	client := &testClientConnector{
		id:       "client-speech",
		done:     make(chan struct{}),
		messages: make(chan connector.Message, 3),
	}
	sess, err := NewSession(context.Background(), testConfig(), client)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close(context.Background(), "test done")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sess.Controller().Emit(ctx, media.StageEvent{
		SessionID: client.ID(),
		Type:      controller.EventSpeechStarted,
		Direction: media.DirectionUplink,
		Stage:     "vad_mock",
	})
	sess.Controller().Emit(ctx, media.StageEvent{
		SessionID: client.ID(),
		Type:      controller.EventSpeechStopped,
		Direction: media.DirectionUplink,
		Stage:     "vad_mock",
	})

	want := []string{connector.MessageSpeechStarted, connector.MessageSpeechStopped}
	for _, eventType := range want {
		expectClientMessageType(t, ctx, client.messages, eventType)
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
	id            string
	done          chan struct{}
	audioOutput   connector.AudioOutput
	messageOutput connector.MessageOutput
	consumed      chan media.Frame
	messages      chan connector.Message
	closed        bool
	mu            sync.Mutex
}

// ID 返回测试客户端 ID。
func (c *testClientConnector) ID() string { return c.id }

// Protocol 返回测试客户端协议名称。
func (c *testClientConnector) Protocol() string { return "test_client" }

// BindAudioOutput 绑定测试客户端收到音频后要推送到的输出端。
func (c *testClientConnector) BindAudioOutput(output connector.AudioOutput) error {
	c.audioOutput = output
	return nil
}

// BindMessageOutput 绑定测试客户端收到消息后要推送到的输出端。
func (c *testClientConnector) BindMessageOutput(output connector.MessageOutput) error {
	c.messageOutput = output
	return nil
}

// SendAudio 记录测试下行音频帧。
func (c *testClientConnector) SendAudio(ctx context.Context, frame media.Frame) error {
	if c.consumed != nil {
		select {
		case c.consumed <- frame:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// SendMessage 记录测试下行消息。
func (c *testClientConnector) SendMessage(ctx context.Context, msg connector.Message) error {
	return c.recordMessage(ctx, msg)
}

// recordMessage 写入测试消息通道。
func (c *testClientConnector) recordMessage(ctx context.Context, msg connector.Message) error {
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

// expectClientMessageType 等待测试客户端收到指定消息类型。
func expectClientMessageType(t *testing.T, ctx context.Context, messages <-chan connector.Message, want string) {
	t.Helper()
	select {
	case msg := <-messages:
		if msg.Type != want {
			t.Fatalf("message type = %q, want %q", msg.Type, want)
		}
		if msg.Direction != media.DirectionDownlink {
			t.Fatalf("message direction = %q", msg.Direction)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", want)
	}
}
