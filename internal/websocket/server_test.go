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
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/pipeline/stages"
	"rtc-media-server/internal/session"
)

// TestStreamAppendJSONEntersSessionUplink 验证 stream append JSON 会进入 Session 上行 pipeline。
func TestStreamAppendJSONEntersSessionUplink(t *testing.T) {
	frameCh := make(chan media.Frame, 1)
	_, url, _ := newTestTLSServer(t, session.Dependencies{
		NewServiceConnector: newCapturingServiceConnector(frameCh),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-a")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	alaw := []byte{0xD5, 0x55}
	if err := conn.Write(ctx, coderws.MessageText, streamAppendJSON(t, alaw)); err != nil {
		t.Fatalf("write stream: %v", err)
	}

	select {
	case frame := <-frameCh:
		if frame.SessionID != "client-a" {
			t.Fatalf("SessionID = %q", frame.SessionID)
		}
		if frame.Direction != media.DirectionUplink {
			t.Fatalf("Direction = %q", frame.Direction)
		}
		if frame.Format.Codec != media.CodecPCM16LE {
			t.Fatalf("Codec = %q", frame.Format.Codec)
		}
		if len(frame.Payload) != len(alaw)*2 {
			t.Fatalf("pcm len = %d", len(frame.Payload))
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for uplink frame")
	}
}

// TestNonAppendStreamJSONDoesNotEmitMedia 验证非 append 事件只上报事件不输出媒体帧。
func TestNonAppendStreamJSONDoesNotEmitMedia(t *testing.T) {
	frameCh := make(chan media.Frame, 1)
	eventCh := make(chan string, 1)
	_, url, _ := newTestTLSServer(t, session.Dependencies{
		NewServiceConnector: newCapturingServiceConnector(frameCh),
		OnEvent: func(_ *session.Session, event connector.Event) {
			eventCh <- event.Type
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-b")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	if err := conn.Write(ctx, coderws.MessageText, []byte(`{"type":"input_audio_buffer.commit"}`)); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	select {
	case got := <-eventCh:
		if got != "input_audio_buffer.commit" {
			t.Fatalf("event type = %q", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for stream event")
	}
	select {
	case <-frameCh:
		t.Fatal("unexpected media frame")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestInvalidStreamJSONReportsError 验证非法 stream JSON 会触发错误回调。
func TestInvalidStreamJSONReportsError(t *testing.T) {
	errCh := make(chan error, 1)
	_, url, _ := newTestTLSServer(t, session.Dependencies{
		OnError: func(_ *session.Session, err error) {
			errCh <- err
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-c")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	if err := conn.Write(ctx, coderws.MessageText, []byte("not-json")); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for error callback")
	}
}

// TestDownlinkPipelineSendsResponseAudioDelta 验证下行 PCM 会发送 response.audio.delta。
func TestDownlinkPipelineSendsResponseAudioDelta(t *testing.T) {
	sessionCh := make(chan *session.Session, 1)
	_, url, _ := newTestTLSServer(t, session.Dependencies{
		OnSession: func(session *session.Session) {
			sessionCh <- session
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-d")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	var sess *session.Session
	select {
	case sess = <-sessionCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for session")
	}

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
	_, url, client := newTestTLSServer(t, session.Dependencies{})

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
	_, url, client := newTestTLSServer(t, session.Dependencies{})

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
	_, url, _ := newTestTLSServer(t, session.Dependencies{
		OnSession: func(session *session.Session) {
			sessionCh <- session
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	streamConn := dialTestWS(t, ctx, url+"/v1/stream", "client-g")
	defer streamConn.Close(coderws.StatusNormalClosure, "test done")

	var sess *session.Session
	select {
	case sess = <-sessionCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for session")
	}
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

// streamAppendJSON 构造端侧上行 append 事件 JSON。
func streamAppendJSON(t *testing.T, alaw []byte) []byte {
	t.Helper()
	payload, err := json.Marshal(struct {
		Type  string `json:"type"`
		Audio string `json:"audio"`
	}{
		Type:  "input_audio_buffer.append",
		Audio: base64.StdEncoding.EncodeToString(alaw),
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// newTestTLSServer 创建使用自签证书的 WebSocket 测试服务。
func newTestTLSServer(t *testing.T, deps session.Dependencies) (*Server, string, *http.Client) {
	t.Helper()

	dir := t.TempDir()
	cert, key := writeSelfSignedCert(t, dir)
	cfg := DefaultConfig()
	cfg.TLS.CertFile = cert
	cfg.TLS.KeyFile = key
	if deps.NewUplinkStages == nil {
		deps.NewUplinkStages = func(sess *session.Session) ([]media.Stage, error) {
			return []media.Stage{
				stages.NewWebSocketJSONUnpack(func(ctx context.Context, frame media.Frame, event connector.Event) {
					sess.OnEvent(ctx, event)
				}),
				stages.NewBase64Decode(),
				stages.NewALawDecode(media.DefaultPCM16Format()),
			}, nil
		}
	}
	if deps.NewDownlinkStages == nil {
		deps.NewDownlinkStages = func(sess *session.Session) ([]media.Stage, error) {
			return []media.Stage{
				stages.NewPCM16Normalizer(media.DefaultPCM16Format()),
				stages.NewALawEncode(),
				stages.NewBase64Encode(),
				stages.NewWebSocketJSONPack(),
			}, nil
		}
	}
	if deps.NewServiceConnector == nil {
		deps.NewServiceConnector = newCapturingServiceConnector(make(chan media.Frame, 1))
	}

	manager := session.NewManager(session.Config{
		UplinkQueueSize:   8,
		DownlinkQueueSize: 8,
		TargetFormat:      media.DefaultPCM16Format(),
	}, deps)
	server, err := NewServer(cfg, manager, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewTLSServer(server.httpSrv.Handler)
	t.Cleanup(ts.Close)
	return server, "wss" + ts.URL[len("https"):], ts.Client()
}

// newCapturingServiceConnector 创建捕获上行帧的服务侧 Connector 工厂。
func newCapturingServiceConnector(frameCh chan<- media.Frame) func(*session.Session) (connector.ServiceConnector, error) {
	return func(sess *session.Session) (connector.ServiceConnector, error) {
		return &capturingServiceConnector{
			id:      sess.ID(),
			frameCh: frameCh,
			done:    make(chan struct{}),
		}, nil
	}
}

// capturingServiceConnector 是测试用服务侧 Connector。
type capturingServiceConnector struct {
	id      string
	frameCh chan<- media.Frame
	done    chan struct{}
	once    sync.Once
}

// ID 返回测试服务侧 Connector ID。
func (c *capturingServiceConnector) ID() string { return c.id }

// Protocol 返回测试服务侧 Connector 协议名称。
func (c *capturingServiceConnector) Protocol() string { return "capturing_service" }

// Start 绑定测试服务侧 Connector 的下行 sink。
func (c *capturingServiceConnector) Start(ctx context.Context, downlink media.Sink) error { return nil }

// Consume 捕获上行 pipeline 输出的媒体帧。
func (c *capturingServiceConnector) Consume(ctx context.Context, frame media.Frame) error {
	c.frameCh <- frame
	return nil
}

// Close 关闭测试服务侧 Connector。
func (c *capturingServiceConnector) Close(ctx context.Context, reason string) error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// Done 返回测试服务侧 Connector 关闭通知。
func (c *capturingServiceConnector) Done() <-chan struct{} { return c.done }

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
