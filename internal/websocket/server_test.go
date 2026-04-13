package websocket

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/session"
)

// TestStreamAppendJSONEntersSessionUplink 验证 stream append JSON 会进入 Session 上行 pipeline。
func TestStreamAppendJSONEntersSessionUplink(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	_, url, _ := newTestTLSServer(t, func(sess *session.Session) {
		sessionCh <- sess
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-a")
	defer conn.Close(coderws.StatusNormalClosure, "test done")
	sess := waitForSession(t, ctx, sessionCh)

	alaw := []byte{0xD5, 0x55}
	if err := conn.Write(ctx, coderws.MessageText, streamAppendJSON(t, alaw)); err != nil {
		t.Fatalf("write stream: %v", err)
	}

	waitForServiceCount(t, ctx, sess, 1)
}

// TestNonAppendStreamJSONDoesNotEmitMedia 验证非 append 事件不会进入媒体处理终点。
func TestNonAppendStreamJSONDoesNotEmitMedia(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	eventCh := make(chan media.Event, 1)
	_, url, _ := newTestTLSServer(t, func(sess *session.Session) {
		sessionCh <- sess
	}, func(callbacks *Callbacks) {
		callbacks.OnEvent = func(ctx context.Context, clientID string, event media.Event) {
			eventCh <- event
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-b")
	defer conn.Close(coderws.StatusNormalClosure, "test done")
	sess := waitForSession(t, ctx, sessionCh)

	if err := conn.Write(ctx, coderws.MessageText, []byte(`{"type":"input_audio_buffer.commit"}`)); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := serviceCount(t, sess); got != 0 {
		t.Fatalf("service count = %d", got)
	}
	if got := waitEvent(t, ctx, eventCh); got.Type != "input_audio_buffer.commit" {
		t.Fatalf("event type = %q", got.Type)
	}
}

// TestInvalidStreamJSONDoesNotBreakSession 验证非法 stream JSON 不会进入媒体终点，且 Session 可继续处理后续帧。
func TestInvalidStreamJSONDoesNotBreakSession(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	_, url, _ := newTestTLSServer(t, func(sess *session.Session) {
		sessionCh <- sess
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-c")
	defer conn.Close(coderws.StatusNormalClosure, "test done")
	sess := waitForSession(t, ctx, sessionCh)

	if err := conn.Write(ctx, coderws.MessageText, []byte("not-json")); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := serviceCount(t, sess); got != 0 {
		t.Fatalf("service count after invalid json = %d", got)
	}

	if err := conn.Write(ctx, coderws.MessageText, streamAppendJSON(t, []byte{0xD5})); err != nil {
		t.Fatalf("write valid stream: %v", err)
	}
	waitForServiceCount(t, ctx, sess, 1)
}

// TestControlStreamJSONEventsAreReported 验证 stream 控制事件会被 Connector 识别并上报，但不会进入媒体 pipeline。
func TestControlStreamJSONEventsAreReported(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	eventCh := make(chan media.Event, 4)
	_, url, _ := newTestTLSServer(t, func(sess *session.Session) {
		sessionCh <- sess
	}, func(callbacks *Callbacks) {
		callbacks.OnEvent = func(ctx context.Context, clientID string, event media.Event) {
			eventCh <- event
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-control")
	defer conn.Close(coderws.StatusNormalClosure, "test done")
	sess := waitForSession(t, ctx, sessionCh)

	payloads := [][]byte{
		[]byte(`{"event_id":"commit-1","type":"input_audio_buffer.commit"}`),
		[]byte(`{"event_id":"create-1","type":"response.create"}`),
		[]byte(`{"event_id":"cancel-1","type":"response.cancel"}`),
		[]byte(`{"event_id":"session-1","type":"session.update","session":{"object":"realtime.session","input_audio_format":"g711_alaw","output_audio_format":"g711_alaw"}}`),
	}
	wantTypes := []string{
		"input_audio_buffer.commit",
		"response.create",
		"response.cancel",
		"session.update",
	}
	for _, payload := range payloads {
		if err := conn.Write(ctx, coderws.MessageText, payload); err != nil {
			t.Fatalf("write stream: %v", err)
		}
	}
	for _, want := range wantTypes {
		if got := waitEvent(t, ctx, eventCh); got.Type != want {
			t.Fatalf("event type = %q, want %q", got.Type, want)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := serviceCount(t, sess); got != 0 {
		t.Fatalf("service count = %d", got)
	}
}

// TestRealtimeRouteAcceptsAuthHeaders 验证端侧 /v1/realtime 路径和认证扩展 headers 能建联。
func TestRealtimeRouteAcceptsAuthHeaders(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	_, url, client := newTestTLSServer(t, func(sess *session.Session) {
		sessionCh <- sess
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	headers := http.Header{}
	headers.Set("X-Auth-Type", "1")
	headers.Set("X-Product-Key", "product-test")
	headers.Set("X-Device-Name", "device-test")
	headers.Set("X-Random-Num", "123456")
	headers.Set("X-Timestamp", "1710000000")
	headers.Set("X-Instance-Id", "instance-test")
	headers.Set("X-Signature", "invalid-signature-is-accepted")
	headers.Set("X-Hardware-Id", "hardware-realtime")
	conn, _, err := coderws.Dial(ctx, url+RealtimeStreamPath+"?bot=bot-test&wait_for_session_update=true", &coderws.DialOptions{
		HTTPClient: client,
		HTTPHeader: headers,
	})
	if err != nil {
		t.Fatalf("dial realtime route: %v", err)
	}
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	sess := waitForSession(t, ctx, sessionCh)
	if sess.ID() != "hardware-realtime" {
		t.Fatalf("session id = %q", sess.ID())
	}
}

// TestStreamControlEventsSendAcks 验证端侧关键控制事件会收到最小状态回包。
func TestStreamControlEventsSendAcks(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	_, url, _ := newTestTLSServer(t, func(sess *session.Session) {
		sessionCh <- sess
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-ack")
	defer conn.Close(coderws.StatusNormalClosure, "test done")
	_ = waitForSession(t, ctx, sessionCh)

	tests := []struct {
		send []byte
		want string
	}{
		{
			send: []byte(`{"event_id":"session-ack","type":"session.update","session":{"object":"realtime.session","input_audio_format":"g711_alaw","output_audio_format":"g711_alaw"}}`),
			want: "session.updated",
		},
		{
			send: []byte(`{"event_id":"commit-ack","type":"input_audio_buffer.commit"}`),
			want: "input_audio_buffer.committed",
		},
		{
			send: []byte(`{"event_id":"create-ack","type":"response.create"}`),
			want: "response.created",
		},
	}
	for _, tt := range tests {
		if err := conn.Write(ctx, coderws.MessageText, tt.send); err != nil {
			t.Fatalf("write stream: %v", err)
		}
		if got := readEventType(t, ctx, conn); got != tt.want {
			t.Fatalf("ack type = %q, want %q", got, tt.want)
		}
	}
}

// TestDownlinkPipelineSendsResponseAudioDelta 验证下行 PCM 会发送 response.audio.delta。
func TestDownlinkPipelineSendsResponseAudioDelta(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	_, url, _ := newTestTLSServer(t, func(sess *session.Session) {
		sessionCh <- sess
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-d")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	sess := waitForSession(t, ctx, sessionCh)

	pcm := []byte{0x00, 0x00, 0x00, 0x01}
	if err := sess.EnqueueDownlink(ctx, media.Frame{Payload: pcm, Format: media.DefaultPCM16Format()}); err != nil {
		t.Fatalf("EnqueueDownlink: %v", err)
	}
	_, got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read sent stream pcm: %v", err)
	}

	var payload struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("sent payload was not json: %v", err)
	}
	if payload.Type != "response.audio.delta" {
		t.Fatalf("type = %q", payload.Type)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload.Delta)
	if err != nil {
		t.Fatalf("delta was not base64: %v", err)
	}
	if len(decoded) != len(pcm)/2 {
		t.Fatalf("alaw len = %d", len(decoded))
	}
}

// TestMissingClientIDRejected 验证缺少客户端 ID Header 时拒绝建联。
func TestMissingClientIDRejected(t *testing.T) {
	_, url, client := newTestTLSServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := coderws.Dial(ctx, url+"/v1/stream", &coderws.DialOptions{
		HTTPClient: client,
	})
	if err == nil {
		t.Fatal("expected dial error")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %#v, err = %v", resp, err)
	}
}

// TestDuplicateChannelRejected 验证同一客户端重复 stream 建联会被拒绝。
func TestDuplicateChannelRejected(t *testing.T) {
	_, url, client := newTestTLSServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := dialTestWS(t, ctx, url+"/v1/stream", "client-f")
	defer first.Close(coderws.StatusNormalClosure, "test done")

	headers := http.Header{}
	headers.Set(DefaultClientIDHeader, "client-f")
	_, resp, err := coderws.Dial(ctx, url+"/v1/stream", &coderws.DialOptions{
		HTTPClient: client,
		HTTPHeader: headers,
	})
	if err == nil {
		t.Fatal("expected duplicate dial error")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %#v, err = %v", resp, err)
	}
}

// TestRTTUsesStreamChannel 验证 RTT 通过 stream channel 测量并缓存。
func TestRTTUsesStreamChannel(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	_, url, _ := newTestTLSServer(t, func(sess *session.Session) {
		sessionCh <- sess
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	streamConn := dialTestWS(t, ctx, url+"/v1/stream", "client-g")
	defer streamConn.Close(coderws.StatusNormalClosure, "test done")

	sess := waitForSession(t, ctx, sessionCh)
	readCtx, readCancel := context.WithCancel(context.Background())
	defer readCancel()
	go func() {
		for {
			_, _, err := streamConn.Read(readCtx)
			if err != nil {
				return
			}
		}
	}()

	rtt, err := sess.MeasureRTT(ctx)
	if err != nil {
		t.Fatalf("MeasureRTT: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("rtt = %s", rtt)
	}
	cached, ok := sess.RTT()
	if !ok || cached <= 0 {
		t.Fatalf("cached rtt = %s ok=%v", cached, ok)
	}
}

// TestOnConnectErrorRejectsConnection 验证 OnConnect 返回错误时连接会被关闭并清理。
func TestOnConnectErrorRejectsConnection(t *testing.T) {
	var attempts atomic.Int32
	cfg, url, client := newBareTestTLSServer(t, Callbacks{
		OnConnect: func(ctx context.Context, client connector.ClientConnector) error {
			if attempts.Add(1) == 1 {
				return errors.New("attach failed")
			}
			return client.BindInput(media.InputFunc(func(ctx context.Context, frame media.Frame) error {
				return nil
			}))
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	headers := http.Header{}
	headers.Set(cfg.ClientIDHeader, "client-h")
	conn, _, err := coderws.Dial(ctx, url+cfg.StreamPath, &coderws.DialOptions{
		HTTPClient: client,
		HTTPHeader: headers,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("expected connection close after OnConnect error")
	}

	conn = dialTestWS(t, ctx, url+cfg.StreamPath, "client-h")
	defer conn.Close(coderws.StatusNormalClosure, "test done")
}

// TestDisconnectCallback 验证 stream 连接断开会触发 OnDisconnect。
func TestDisconnectCallback(t *testing.T) {
	disconnectedCh := make(chan string, 1)
	_, url, _ := newTestTLSServer(t, nil, func(callbacks *Callbacks) {
		callbacks.OnDisconnect = func(ctx context.Context, clientID string, err error) {
			disconnectedCh <- clientID
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-i")
	if err := conn.Close(coderws.StatusNormalClosure, "test done"); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case got := <-disconnectedCh:
		if got != "client-i" {
			t.Fatalf("clientID = %q", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for disconnect callback")
	}
}

// streamAppendJSON 构造端侧上行 append 事件 JSON。
func streamAppendJSON(t *testing.T, alaw []byte) []byte {
	t.Helper()
	payload, err := json.Marshal(struct {
		EventID string `json:"event_id"`
		Type    string `json:"type"`
		Audio   string `json:"audio"`
	}{
		EventID: "append-test",
		Type:    "input_audio_buffer.append",
		Audio:   base64.StdEncoding.EncodeToString(alaw),
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// newTestTLSServer 创建使用自签证书和真实 Session 链路的 WebSocket 测试服务。
func newTestTLSServer(t *testing.T, onSession func(*session.Session), callbackOpts ...func(*Callbacks)) (*Server, string, *http.Client) {
	t.Helper()

	dir := t.TempDir()
	cert, key := writeSelfSignedCert(t, dir)
	cfg := DefaultConfig()
	cfg.TLS.CertFile = cert
	cfg.TLS.KeyFile = key
	manager := session.NewManager(session.Config{
		UplinkQueueSize:   8,
		DownlinkQueueSize: 8,
		TargetFormat:      media.DefaultPCM16Format(),
	})
	callbacks := Callbacks{
		OnConnect: func(ctx context.Context, client connector.ClientConnector) error {
			sess, _, err := manager.Attach(ctx, client)
			if err == nil && onSession != nil {
				onSession(sess)
			}
			return err
		},
		OnDisconnect: func(ctx context.Context, clientID string, err error) {
			manager.Remove(ctx, clientID, err)
		},
		OnError: func(ctx context.Context, clientID string, err error) {
			if sess, ok := manager.Get(clientID); ok {
				sess.OnError(ctx, err)
			}
		},
		OnEvent: func(ctx context.Context, clientID string, event media.Event) {
			if sess, ok := manager.Get(clientID); ok {
				sess.OnEvent(ctx, event)
			}
		},
	}
	for _, opt := range callbackOpts {
		opt(&callbacks)
	}
	server, err := NewServer(cfg, callbacks)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewTLSServer(server.httpSrv.Handler)
	t.Cleanup(ts.Close)
	return server, "wss" + ts.URL[len("https"):], ts.Client()
}

// waitEvent 等待 WebSocket connector 上报 stream 事件。
func waitEvent(t *testing.T, ctx context.Context, eventCh <-chan media.Event) media.Event {
	t.Helper()
	select {
	case event := <-eventCh:
		return event
	case <-ctx.Done():
		t.Fatal("timed out waiting for stream event")
		return media.Event{}
	}
}

// readEventType 读取一条 WebSocket JSON 消息并返回 type 字段。
func readEventType(t *testing.T, ctx context.Context, conn *coderws.Conn) string {
	t.Helper()
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("unmarshal event %s: %v", payload, err)
	}
	return event.Type
}

// newBareTestTLSServer 创建不带 SessionManager 的 WebSocket 测试服务。
func newBareTestTLSServer(t *testing.T, callbacks Callbacks) (Config, string, *http.Client) {
	t.Helper()

	dir := t.TempDir()
	cert, key := writeSelfSignedCert(t, dir)
	cfg := DefaultConfig()
	cfg.TLS.CertFile = cert
	cfg.TLS.KeyFile = key
	server, err := NewServer(cfg, callbacks)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewTLSServer(server.httpSrv.Handler)
	t.Cleanup(ts.Close)
	return cfg, "wss" + ts.URL[len("https"):], ts.Client()
}

// waitForSession 等待测试 WebSocket 建联后创建出的 Session。
func waitForSession(t *testing.T, ctx context.Context, sessionCh <-chan *session.Session) *session.Session {
	t.Helper()
	select {
	case sess := <-sessionCh:
		return sess
	case <-ctx.Done():
		t.Fatal("timed out waiting for session")
	}
	return nil
}

// serviceCount 返回真实 mock service connector 已消费的上行帧数量。
func serviceCount(t *testing.T, sess *session.Session) uint64 {
	t.Helper()
	counter, ok := sess.ServiceConnector().(interface{ Count() uint64 })
	if !ok {
		t.Fatalf("service connector %T does not expose Count", sess.ServiceConnector())
	}
	return counter.Count()
}

// waitForServiceCount 等待真实 mock service connector 消费到指定帧数。
func waitForServiceCount(t *testing.T, ctx context.Context, sess *session.Session, want uint64) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := serviceCount(t, sess); got >= want {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for service count %d, got %d", want, serviceCount(t, sess))
		}
	}
}

// dialTestWS 建立带客户端 ID Header 的测试 WebSocket 连接。
func dialTestWS(t *testing.T, ctx context.Context, url string, clientID string) *coderws.Conn {
	t.Helper()

	headers := http.Header{}
	headers.Set(DefaultClientIDHeader, clientID)
	conn, _, err := coderws.Dial(ctx, url, &coderws.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		HTTPHeader: headers,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

// writeSelfSignedCert 写入测试用自签 TLS 证书。
func writeSelfSignedCert(t *testing.T, dir string) (string, string) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:    []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}

	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	certOut := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	if len(certOut) == 0 || len(keyOut) == 0 {
		t.Fatal(errors.New("failed to encode certificate"))
	}
	if err := os.WriteFile(certPath, certOut, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyOut, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
