package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/session"
	"rtc-media-server/internal/websocket"
)

const configPath = "configs/config.yaml"

// main 组装 demo 运行所需的配置、SessionManager 和 WebSocket 服务。
func main() {
	logger := slog.Default()

	cfg, err := websocket.LoadConfig(configPath)
	if err != nil {
		fatal(logger, "加载配置失败", err)
	}

	if err := ensureDemoCertificate(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
		fatal(logger, "准备本地 WSS 证书失败", err)
	}

	sessionManager := session.NewManager(session.Config{
		UplinkQueueSize:   32,
		DownlinkQueueSize: 32,
		CloseTimeout:      3 * time.Second,
		TargetFormat:      media.DefaultPCM16Format(),
		Logger:            logger,
	})

	server, err := websocket.NewServer(cfg, websocket.Callbacks{
		OnConnect: func(ctx context.Context, client connector.ClientConnector) error {
			_, _, err := sessionManager.Attach(ctx, client)
			return err
		},
		OnDisconnect: func(ctx context.Context, clientID string, err error) {
			sessionManager.Remove(ctx, clientID, err)
		},
		OnError: func(ctx context.Context, clientID string, err error) {
			if sess, ok := sessionManager.Get(clientID); ok {
				sess.OnError(ctx, err)
				return
			}
			logger.Error(
				"client_id="+clientID+" websocket error",
				slog.String("client_id", clientID),
				slog.Any("error", err),
			)
		},
	}, logger)
	if err != nil {
		fatal(logger, "创建 WebSocket 服务失败", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := net.JoinHostPort(cfg.Listen, strconv.Itoa(cfg.Port))
	logger.Info(
		"rtc-media-server demo 已启动",
		slog.String("stream_addr", "wss://"+addr+cfg.StreamPath),
	)
	if err := server.Start(ctx); err != nil {
		fatal(logger, "WebSocket 服务退出", err)
	}
	logger.Info("rtc-media-server demo 已停止")
}

// ensureDemoCertificate 确保本地 demo WSS 证书存在，不存在时生成自签证书。
func ensureDemoCertificate(certFile, keyFile string) error {
	certExists := fileExists(certFile)
	keyExists := fileExists(keyFile)
	if certExists && keyExists {
		return nil
	}
	if certExists != keyExists {
		slog.Warn("检测到证书或私钥缺失，将重新生成本地 demo 证书", slog.String("cert_file", certFile), slog.String("key_file", keyFile))
	}

	if err := os.MkdirAll(filepath.Dir(certFile), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o755); err != nil {
		return err
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: "rtc-media-server-demo",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("0.0.0.0")},
		DNSNames:    []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return errors.New("编码本地 demo 证书失败")
	}
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return err
	}
	slog.Info("已生成本地 demo WSS 证书", slog.String("cert_file", certFile), slog.String("key_file", keyFile))
	return nil
}

// fileExists 判断指定路径是否存在。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fatal 记录致命错误并退出进程。
func fatal(logger *slog.Logger, msg string, err error) {
	logger.Error(msg, slog.Any("error", err))
	os.Exit(1)
}
