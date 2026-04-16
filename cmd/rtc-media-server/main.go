package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/pipeline"
	"rtc-media-server/internal/session"
	"rtc-media-server/internal/websocket"
)

const (
	configPath = "configs/websocket.yaml"

	demoRSAKeyBits     = 2048
	demoCertValidDays  = 365
	demoCertDirPerm    = 0o755
	demoCertFilePerm   = 0o600
	demoCertLoopbackIP = "127.0.0.1"
	demoCertWildcardIP = "0.0.0.0"
	demoCertLocalhost  = "localhost"
	demoCertCommonName = "rtc-media-server-demo"
	demoCertPEMType    = "CERTIFICATE"
	demoPrivateKeyType = "RSA PRIVATE KEY"
	exitFailure        = 1
)

// main 组装 demo 运行所需的配置、SessionManager 和 WebSocket 服务。
func main() {
	cfg, err := websocket.LoadConfig(configPath)
	if err != nil {
		fatal("load config failed", err)
	}

	if err := ensureDemoCertificate(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
		fatal("prepare local WSS certificate failed", err)
	}

	sessionManager := session.NewManager(session.Config{
		UplinkQueueSize:   pipeline.DefaultQueueSize,
		DownlinkQueueSize: pipeline.DefaultQueueSize,
		CloseTimeout:      session.DefaultCloseTimeout,
		TargetFormat:      media.DefaultPCM16Format(),
	})

	server, err := websocket.NewServer(cfg, websocket.Callbacks{
		OnConnect: func(ctx context.Context, client connector.ClientConnector) error {
			_, _, err := sessionManager.Attach(ctx, client)
			return err
		},
		OnDisconnect: func(ctx context.Context, clientID string, err error) {
			sessionManager.Remove(ctx, clientID, err)
		},
		OnEvent: func(ctx context.Context, clientID string, event media.Event) {
			if sess, ok := sessionManager.Get(clientID); ok {
				sess.OnEvent(ctx, event)
			}
		},
		OnResponseAudio: func(ctx context.Context, clientID string, frame media.Frame) error {
			sess, ok := sessionManager.Get(clientID)
			if !ok {
				return nil
			}
			return sess.EnqueueDownlink(ctx, frame)
		},
		OnRTT: func(ctx context.Context, clientID string, rtt time.Duration) {
			if sess, ok := sessionManager.Get(clientID); ok {
				sess.UpdateRTT(rtt)
			}
		},
		OnError: func(ctx context.Context, clientID string, err error) {
			if sess, ok := sessionManager.Get(clientID); ok {
				sess.OnError(ctx, err)
				return
			}
			log.Errorf("client_id=%s websocket error: %v", clientID, err)
		},
	})
	if err != nil {
		fatal("create WebSocket server failed", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := net.JoinHostPort(cfg.Listen, strconv.Itoa(cfg.Port))
	log.Infof("rtc-media-server demo started realtime_addr=wss://%s%s", addr, cfg.StreamPath)
	if err := server.Start(ctx); err != nil {
		fatal("WebSocket server exited", err)
	}
	log.Infof("rtc-media-server demo stopped")
}

// ensureDemoCertificate 确保本地 demo WSS 证书存在，不存在时生成自签证书。
func ensureDemoCertificate(certFile, keyFile string) error {
	certExists := fileExists(certFile)
	keyExists := fileExists(keyFile)
	if certExists && keyExists {
		return nil
	}
	if certExists != keyExists {
		log.Warnf("demo certificate or key is missing, generating local demo certificate cert_file=%s key_file=%s", certFile, keyFile)
	}

	if err := os.MkdirAll(filepath.Dir(certFile), demoCertDirPerm); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), demoCertDirPerm); err != nil {
		return err
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, demoRSAKeyBits)
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: demoCertCommonName,
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(demoCertValidDays * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{net.ParseIP(demoCertLoopbackIP), net.ParseIP(demoCertWildcardIP)},
		DNSNames:    []string{demoCertLocalhost},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: demoCertPEMType, Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: demoPrivateKeyType, Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return errors.New("encode local demo certificate failed")
	}
	if err := os.WriteFile(certFile, certPEM, demoCertFilePerm); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, keyPEM, demoCertFilePerm); err != nil {
		return err
	}
	log.Infof("local demo WSS certificate generated cert_file=%s key_file=%s", certFile, keyFile)
	return nil
}

// fileExists 判断指定路径是否存在。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fatal 记录致命错误并退出进程。
func fatal(msg string, err error) {
	log.Errorf("%s: %v", msg, err)
	os.Exit(exitFailure)
}
