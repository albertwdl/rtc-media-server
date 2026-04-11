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
)

func TestSessionLifecycleBindsStreamAndCmd(t *testing.T) {
	var (
		mu       sync.Mutex
		sessions []*Session
	)
	server, url, _ := newTestTLSServer(t, ServerCallbacks{
		OnSession: func(session *Session) {
			mu.Lock()
			defer mu.Unlock()
			sessions = append(sessions, session)
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	streamConn := dialTestWS(t, ctx, url+"/v1/stream", "client-a")
	defer streamConn.Close(coderws.StatusNormalClosure, "test done")
	cmdConn := dialTestWS(t, ctx, url+"/v1/cmd", "client-a")
	defer cmdConn.Close(coderws.StatusNormalClosure, "test done")

	session, ok := server.Session("client-a")
	if !ok {
		t.Fatal("expected session")
	}
	if session.ID() != "client-a" {
		t.Fatalf("session id = %q", session.ID())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 1 {
		t.Fatalf("OnSession calls = %d", len(sessions))
	}
	if sessions[0] != session {
		t.Fatal("OnSession returned a different session")
	}
}

func TestSessionCallbacksAreIsolated(t *testing.T) {
	type gotEvent struct {
		clientID string
		kind     string
	}
	events := make(chan gotEvent, 4)
	_, url, _ := newTestTLSServer(t, ServerCallbacks{
		OnSession: func(session *Session) {
			session.SetCallbacks(SessionCallbacks{
				OnStreamPCM: func(session *Session, _ []byte) {
					events <- gotEvent{clientID: session.ID(), kind: "pcm"}
				},
				OnCommand: func(session *Session, _ []byte) {
					events <- gotEvent{clientID: session.ID(), kind: "cmd"}
				},
			})
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientA := dialTestWS(t, ctx, url+"/v1/stream", "client-a")
	defer clientA.Close(coderws.StatusNormalClosure, "test done")
	clientB := dialTestWS(t, ctx, url+"/v1/cmd", "client-b")
	defer clientB.Close(coderws.StatusNormalClosure, "test done")

	alaw := []byte{0xD5, 0x55}
	msg := streamAppendJSON(t, alaw)
	if err := clientA.Write(ctx, coderws.MessageText, msg); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	if err := clientB.Write(ctx, coderws.MessageText, []byte(`{"action":"start"}`)); err != nil {
		t.Fatalf("write cmd: %v", err)
	}

	got := map[gotEvent]bool{}
	for len(got) < 2 {
		select {
		case event := <-events:
			got[event] = true
		case <-ctx.Done():
			t.Fatal("timed out waiting for callbacks")
		}
	}
	if !got[gotEvent{clientID: "client-a", kind: "pcm"}] {
		t.Fatalf("missing client-a pcm callback: %#v", got)
	}
	if !got[gotEvent{clientID: "client-b", kind: "cmd"}] {
		t.Fatalf("missing client-b cmd callback: %#v", got)
	}
}

func TestStreamAppendJSONTriggersEventAndPCM(t *testing.T) {
	eventCh := make(chan StreamEvent, 1)
	pcmCh := make(chan []byte, 1)
	_, url, _ := newTestTLSServer(t, ServerCallbacks{
		OnSession: func(session *Session) {
			session.SetCallbacks(SessionCallbacks{
				OnStreamEvent: func(_ *Session, event StreamEvent) {
					eventCh <- event
				},
				OnStreamPCM: func(_ *Session, pcm []byte) {
					pcmCh <- pcm
				},
			})
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-c")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	alaw := []byte{0xD5, 0x55}
	if err := conn.Write(ctx, coderws.MessageText, streamAppendJSON(t, alaw)); err != nil {
		t.Fatalf("write stream: %v", err)
	}

	select {
	case event := <-eventCh:
		if event.Type != "input_audio_buffer.append" {
			t.Fatalf("event type = %q", event.Type)
		}
		if event.AudioBase64 == "" {
			t.Fatal("expected audio base64")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for stream event")
	}
	select {
	case pcm := <-pcmCh:
		if len(pcm) != len(alaw)*2 {
			t.Fatalf("pcm len = %d", len(pcm))
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for pcm")
	}
}

func TestNonAppendStreamJSONOnlyTriggersEvent(t *testing.T) {
	eventCh := make(chan StreamEvent, 1)
	pcmCh := make(chan []byte, 1)
	_, url, _ := newTestTLSServer(t, ServerCallbacks{
		OnSession: func(session *Session) {
			session.SetCallbacks(SessionCallbacks{
				OnStreamEvent: func(_ *Session, event StreamEvent) {
					eventCh <- event
				},
				OnStreamPCM: func(_ *Session, pcm []byte) {
					pcmCh <- pcm
				},
			})
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-d")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	if err := conn.Write(ctx, coderws.MessageText, []byte(`{"type":"input_audio_buffer.commit"}`)); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	select {
	case event := <-eventCh:
		if event.Type != "input_audio_buffer.commit" {
			t.Fatalf("event type = %q", event.Type)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for stream event")
	}
	select {
	case <-pcmCh:
		t.Fatal("unexpected pcm callback")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestInvalidStreamJSONReportsSessionError(t *testing.T) {
	errCh := make(chan error, 1)
	_, url, _ := newTestTLSServer(t, ServerCallbacks{
		OnSession: func(session *Session) {
			session.SetCallbacks(SessionCallbacks{
				OnError: func(_ *Session, err error) {
					errCh <- err
				},
			})
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-e")
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

func TestSessionSendStreamPCMEncodesResponseAudioDelta(t *testing.T) {
	sessionCh := make(chan *Session, 1)
	_, url, _ := newTestTLSServer(t, ServerCallbacks{
		OnSession: func(session *Session) {
			sessionCh <- session
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/stream", "client-f")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	var session *Session
	select {
	case session = <-sessionCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for session")
	}

	pcm := []byte{0x00, 0x00, 0x00, 0x01}
	if err := session.SendStreamPCM(ctx, pcm); err != nil {
		t.Fatalf("SendStreamPCM: %v", err)
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

func TestCommandCallbackAndSessionSendCommand(t *testing.T) {
	cmdCh := make(chan []byte, 1)
	sessionCh := make(chan *Session, 1)
	_, url, _ := newTestTLSServer(t, ServerCallbacks{
		OnSession: func(session *Session) {
			session.SetCallbacks(SessionCallbacks{
				OnCommand: func(_ *Session, payload []byte) {
					cmdCh <- payload
				},
			})
			sessionCh <- session
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dialTestWS(t, ctx, url+"/v1/cmd", "client-g")
	defer conn.Close(coderws.StatusNormalClosure, "test done")

	if err := conn.Write(ctx, coderws.MessageText, []byte(`{"action":"start"}`)); err != nil {
		t.Fatalf("write cmd: %v", err)
	}
	select {
	case got := <-cmdCh:
		if string(got) != `{"action":"start"}` {
			t.Fatalf("cmd payload = %s", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for cmd callback")
	}

	session := <-sessionCh
	if err := session.SendCommand(ctx, []byte(`{"action":"stop"}`)); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	_, got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read sent command: %v", err)
	}
	if string(got) != `{"action":"stop"}` {
		t.Fatalf("sent command = %s", got)
	}
}

func TestMissingClientIDRejected(t *testing.T) {
	_, url, client := newTestTLSServer(t, ServerCallbacks{})

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

func TestDuplicateChannelRejected(t *testing.T) {
	_, url, client := newTestTLSServer(t, ServerCallbacks{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := dialTestWS(t, ctx, url+"/v1/stream", "client-h")
	defer first.Close(coderws.StatusNormalClosure, "test done")

	headers := http.Header{}
	headers.Set(DefaultClientIDHeader, "client-h")
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

func TestRTTUsesOnlyStreamChannel(t *testing.T) {
	server, url, _ := newTestTLSServer(t, ServerCallbacks{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmdConn := dialTestWS(t, ctx, url+"/v1/cmd", "client-i")
	defer cmdConn.Close(coderws.StatusNormalClosure, "test done")

	session, ok := server.Session("client-i")
	if !ok {
		t.Fatal("expected session")
	}
	if _, err := session.MeasureRTT(ctx); err == nil {
		t.Fatal("expected stream channel missing error")
	}

	streamConn := dialTestWS(t, ctx, url+"/v1/stream", "client-i")
	defer streamConn.Close(coderws.StatusNormalClosure, "test done")
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

	rtt, err := server.MeasureRTT(ctx, "client-i")
	if err != nil {
		t.Fatalf("MeasureRTT: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("rtt = %s", rtt)
	}
	cached, ok := session.RTT()
	if !ok || cached <= 0 {
		t.Fatalf("cached rtt = %s ok=%v", cached, ok)
	}

	if err := session.Disconnect(ctx, "test disconnect"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

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

func newTestTLSServer(t *testing.T, callbacks ServerCallbacks) (*Server, string, *http.Client) {
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
	return server, "wss" + ts.URL[len("https"):], ts.Client()
}

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
