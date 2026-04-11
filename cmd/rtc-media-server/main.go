package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"rtc-media-server/internal/websocket"
)

const configPath = "configs/config.yaml"

func main() {
	cfg, err := websocket.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	if err := ensureDemoCertificate(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
		log.Fatalf("准备本地 WSS 证书失败: %v", err)
	}

	server, err := websocket.NewServer(cfg, websocket.ServerCallbacks{
		OnSession: bindSessionCallbacks,
		OnError: func(err error) {
			log.Printf("WebSocket 服务错误: %v", err)
		},
	})
	if err != nil {
		log.Fatalf("创建 WebSocket 服务失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := net.JoinHostPort(cfg.Listen, strconv.Itoa(cfg.Port))
	log.Printf("rtc-media-server demo 已启动: wss://%s%s, cmd: wss://%s%s", addr, cfg.StreamPath, addr, cfg.CmdPath)
	if err := server.Start(ctx); err != nil {
		log.Fatalf("WebSocket 服务退出: %v", err)
	}
	log.Println("rtc-media-server demo 已停止")
}

func bindSessionCallbacks(session *websocket.Session) {
	log.Printf("客户端已连接: %s", session.ID())
	session.SetCallbacks(websocket.SessionCallbacks{
		OnStreamEvent: func(session *websocket.Session, event websocket.StreamEvent) {
			log.Printf("stream事件 client=%s type=%s payload_bytes=%d", session.ID(), event.Type, len(event.RawJSON))
		},
		OnStreamPCM: func(session *websocket.Session, pcm []byte) {
			log.Printf("stream PCM client=%s bytes=%d", session.ID(), len(pcm))
		},
		OnCommand: func(session *websocket.Session, payload []byte) {
			log.Printf("cmd消息 client=%s payload=%s", session.ID(), string(payload))
		},
		OnDisconnected: func(session *websocket.Session, err error) {
			log.Printf("客户端已断开: %s err=%v", session.ID(), err)
		},
		OnError: func(session *websocket.Session, err error) {
			log.Printf("客户端错误: %s err=%v", session.ID(), err)
		},
	})
}

func ensureDemoCertificate(certFile, keyFile string) error {
	certExists := fileExists(certFile)
	keyExists := fileExists(keyFile)
	if certExists && keyExists {
		return nil
	}
	if certExists != keyExists {
		log.Printf("检测到证书或私钥缺失，将重新生成本地 demo 证书: cert=%s key=%s", certFile, keyFile)
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
	log.Printf("已生成本地 demo WSS 证书: cert=%s key=%s", certFile, keyFile)
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
