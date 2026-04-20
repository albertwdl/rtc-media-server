package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestLoadFullConfig 验证完整 YAML 能加载为应用运行配置并写入单例。
func TestLoadFullConfig(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	cert, key := writeTLSFiles(t)
	path := writeConfig(t, `
server:
  port: 8161
  host: "0.0.0.0"
log:
  level: "debug"
  format: "json"
  file: "logs/test.log"
signaling:
  url: "http://127.0.0.1:18084"
  reconnection_attempts: 7
  reconnection_interval_ms: 1500
webrtc:
  enabled: true
  room_id: "room-1"
  token: "token-1"
  port_min: 12000
  port_max: 12100
tls:
  enabled: true
  cert_file: "certs/server.crt"
  key_file: "certs/server.key"
  client_ca_file: "certs/ca.crt"
agent:
  protocol: "wss"
  host: "agent.local"
  port: 8160
  path: "/v1/ai-edge-agent/omni/session"
  skip_verify: true
  ca_cert_file: ""
  cipher_suites: "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"
  min_version: ""
  image_timeout: 10
  tts_sample_rate: 24000
websocket:
  listen: "127.0.0.1"
  port: 9443
  stream_path: "/v1/realtime"
  client_id_header: "X-Hardware-Id"
  tls:
    cert_file: "`+cert+`"
    key_file: "`+key+`"
  rtt_interval: "5s"
  read_timeout: "20s"
  write_timeout: "3s"
  max_message_bytes: 4096
  stream:
    sample_rate: 8000
    channels: 1
session:
  close_timeout: "4s"
  idle_timeout: "1s"
controller:
  initial_silence_timeout: "11s"
  silence_timeout: "6s"
  reference_queue_size: 9
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebSocket.Port != 9443 || cfg.WebSocket.RTTInterval.Duration() != 5*time.Second {
		t.Fatalf("websocket config = %+v", cfg.WebSocket)
	}
	if cfg.Session.CloseTimeout.Duration() != 4*time.Second {
		t.Fatalf("session close timeout = %s", cfg.Session.CloseTimeout)
	}
	if cfg.Session.IdleTimeout.Duration() != time.Second {
		t.Fatalf("session idle timeout = %s", cfg.Session.IdleTimeout)
	}
	if cfg.Controller.InitialSilenceTimeout.Duration() != 11*time.Second || cfg.Controller.ReferenceQueueSize != 9 {
		t.Fatalf("controller config = %+v", cfg.Controller)
	}
	if cfg.Server.Port != 8161 || cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("server config = %+v", cfg.Server)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" || cfg.Log.File != "logs/test.log" {
		t.Fatalf("log config = %+v", cfg.Log)
	}
	if cfg.Signaling.URL != "http://127.0.0.1:18084" || cfg.Signaling.ReconnectionAttempts != 7 || cfg.Signaling.ReconnectionIntervalMS != 1500 {
		t.Fatalf("signaling config = %+v", cfg.Signaling)
	}
	if !cfg.WebRTC.Enabled || cfg.WebRTC.RoomID != "room-1" || cfg.WebRTC.PortMin != 12000 || cfg.WebRTC.PortMax != 12100 {
		t.Fatalf("webrtc config = %+v", cfg.WebRTC)
	}
	if !cfg.TLS.Enabled || cfg.TLS.ClientCAFile != "certs/ca.crt" {
		t.Fatalf("tls config = %+v", cfg.TLS)
	}
	if cfg.Agent.Host != "agent.local" || cfg.Agent.CipherSuites != "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256" || cfg.Agent.TTSSampleRate != 24000 {
		t.Fatalf("agent config = %+v", cfg.Agent)
	}

	got := Get()
	if got.WebSocket.Port != cfg.WebSocket.Port {
		t.Fatalf("Get WebSocket port = %d", got.WebSocket.Port)
	}
}

// TestLoadPartialConfigUsesDefaults 验证局部 YAML 会补齐默认值。
func TestLoadPartialConfigUsesDefaults(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	cert, key := writeTLSFiles(t)
	path := writeConfig(t, minimalConfig(cert, key))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebSocket.Port != DefaultPort {
		t.Fatalf("port = %d", cfg.WebSocket.Port)
	}
	if cfg.Session.CloseTimeout.Duration() != DefaultCloseTimeout {
		t.Fatalf("close timeout = %s", cfg.Session.CloseTimeout)
	}
	if cfg.Controller.SilenceTimeout.Duration() != DefaultSilenceTimeout {
		t.Fatalf("silence timeout = %s", cfg.Controller.SilenceTimeout)
	}
	if cfg.WebSocket.Stream.SampleRate != 8000 || cfg.WebSocket.Stream.Channels != 1 {
		t.Fatalf("stream = %+v", cfg.WebSocket.Stream)
	}
	if cfg.Server.Host != DefaultServerHost || cfg.Server.Port != DefaultServerPort {
		t.Fatalf("server defaults = %+v", cfg.Server)
	}
	if cfg.Log.Level != defaultLogLevel || cfg.Log.Format != defaultLogFormat || cfg.Log.File != defaultLogFile {
		t.Fatalf("log defaults = %+v", cfg.Log)
	}
	if cfg.Signaling.URL != DefaultSignalingURL || cfg.Signaling.ReconnectionIntervalMS != 3000 {
		t.Fatalf("signaling defaults = %+v", cfg.Signaling)
	}
	if !cfg.TLS.Enabled || cfg.TLS.CertFile != defaultTLSCertFile || cfg.TLS.ClientCAFile != defaultTLSClientCA {
		t.Fatalf("tls defaults = %+v", cfg.TLS)
	}
	if cfg.Agent.Protocol != DefaultAgentProtocol || cfg.Agent.Port != DefaultAgentPort || cfg.Agent.TTSSampleRate != DefaultTTSSampleRate {
		t.Fatalf("agent defaults = %+v", cfg.Agent)
	}
	if cfg.WebRTC.RoomID != DefaultWebRTCRoomID || cfg.WebRTC.PortMin != DefaultWebRTCPortMin {
		t.Fatalf("webrtc defaults = %+v", cfg.WebRTC)
	}
}

// TestLoadInvalidConfig 验证关键非法配置会返回错误。
func TestLoadInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		body func(cert, key string) string
	}{
		{
			name: "duration",
			body: func(cert, key string) string {
				return minimalConfig(cert, key) + `session: {close_timeout: "bad"}`
			},
		},
		{
			name: "websocket port",
			body: func(cert, key string) string {
				return `
websocket:
  port: 70000
  tls:
    cert_file: "` + cert + `"
    key_file: "` + key + `"
`
			},
		},
		{
			name: "server port",
			body: func(cert, key string) string {
				return minimalConfig(cert, key) + `
server:
  port: 70000
`
			},
		},
		{
			name: "agent port",
			body: func(cert, key string) string {
				return minimalConfig(cert, key) + `
agent:
  port: 0
`
			},
		},
		{
			name: "webrtc port range",
			body: func(cert, key string) string {
				return minimalConfig(cert, key) + `
webrtc:
  port_min: 12000
  port_max: 11000
`
			},
		},
		{
			name: "top level tls enabled without key",
			body: func(cert, key string) string {
				return minimalConfig(cert, key) + `
tls:
  enabled: true
  cert_file: "server.crt"
  key_file: ""
`
			},
		},
		{
			name: "sample rate",
			body: func(cert, key string) string {
				return `
websocket:
  tls:
    cert_file: "` + cert + `"
    key_file: "` + key + `"
  stream:
    sample_rate: 0
`
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ResetForTest()
			t.Cleanup(ResetForTest)
			cert, key := writeTLSFiles(t)
			path := writeConfig(t, tt.body(cert, key))
			if _, err := Load(path); err == nil {
				t.Fatal("Load returned nil error")
			}
		})
	}
}

// TestGetPanicsBeforeLoad 验证未初始化时读取单例会暴露启动顺序错误。
func TestGetPanicsBeforeLoad(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	defer func() {
		if recover() == nil {
			t.Fatal("Get did not panic before Load")
		}
	}()
	_ = Get()
}

// TestLoadOnlyOnce 验证成功加载后不能重复初始化。
func TestLoadOnlyOnce(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	cert, key := writeTLSFiles(t)
	path := writeConfig(t, minimalConfig(cert, key))
	if _, err := Load(path); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("second Load returned nil error")
	}
	ResetForTest()
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after ResetForTest: %v", err)
	}
}

// TestValidateWebSocketConfig 验证 WebSocket 运行配置校验。
func TestValidateWebSocketConfig(t *testing.T) {
	cert, key := writeTLSFiles(t)
	cfg := DefaultWebSocketConfig()
	cfg.TLS.CertFile = cert
	cfg.TLS.KeyFile = key
	if err := ValidateWebSocketConfig(cfg); err != nil {
		t.Fatalf("ValidateWebSocketConfig: %v", err)
	}
	if err := ValidateWebSocketTLSFiles(cfg); err != nil {
		t.Fatalf("ValidateWebSocketTLSFiles: %v", err)
	}
}

// TestConfigYAMLTags 验证对外配置结构使用显式 YAML 字段名。
func TestConfigYAMLTags(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{Port: 8161, Host: "0.0.0.0"},
		Log:    LogConfig{Level: "debug", Format: "text", File: "logs/test.log"},
		Signaling: SignalingConfig{
			URL:                    "http://127.0.0.1:18084",
			ReconnectionAttempts:   5,
			ReconnectionIntervalMS: 3000,
		},
		WebRTC: WebRTCConfig{Enabled: true, RoomID: "005", Token: "12345", PortMin: 10000, PortMax: 11000},
		TLS:    TLSConfig{Enabled: true, CertFile: "server.crt", KeyFile: "server.key", ClientCAFile: "ca.crt"},
		Agent: AgentConfig{
			Protocol:      "wss",
			Host:          "localhost",
			Port:          8160,
			Path:          "/agent",
			SkipVerify:    true,
			CACertFile:    "",
			CipherSuites:  "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			MinVersion:    "",
			ImageTimeout:  10,
			TTSSampleRate: 24000,
		},
		WebSocket: WebSocketConfig{
			Listen:          "127.0.0.1",
			Port:            9443,
			StreamPath:      "/v1/realtime",
			ClientIDHeader:  "X-Hardware-Id",
			TLS:             WebSocketTLSConfig{CertFile: "server.crt", KeyFile: "server.key"},
			RTTInterval:     Duration(5 * time.Second),
			ReadTimeout:     Duration(20 * time.Second),
			WriteTimeout:    Duration(3 * time.Second),
			MaxMessageBytes: 4096,
			Stream:          StreamConfig{SampleRate: 8000, Channels: 1},
		},
		Session: SessionConfig{
			CloseTimeout: Duration(4 * time.Second),
			IdleTimeout:  Duration(time.Second),
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := string(data)
	for _, want := range []string{
		"server:",
		"log:",
		"file:",
		"signaling:",
		"reconnection_attempts:",
		"reconnection_interval_ms:",
		"webrtc:",
		"room_id:",
		"port_min:",
		"port_max:",
		"client_ca_file:",
		"agent:",
		"skip_verify:",
		"ca_cert_file:",
		"cipher_suites:",
		"min_version:",
		"image_timeout:",
		"tts_sample_rate:",
		"websocket:",
		"stream_path:",
		"client_id_header:",
		"max_message_bytes:",
		"cert_file:",
		"key_file:",
		"sample_rate:",
		"session:",
		"close_timeout:",
		"idle_timeout:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("marshaled YAML missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"WebSocket:", "StreamPath:", "ClientIDHeader:", "CertFile:"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("marshaled YAML contains untagged field %q:\n%s", unwanted, out)
		}
	}
}

func writeTLSFiles(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cert := filepath.Join(dir, "server.crt")
	key := filepath.Join(dir, "server.key")
	if err := os.WriteFile(cert, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func minimalConfig(cert, key string) string {
	return `
websocket:
  tls:
    cert_file: "` + cert + `"
    key_file: "` + key + `"
`
}
