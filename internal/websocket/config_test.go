package websocket

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadConfig 验证 YAML 配置能加载为运行时 WebSocket 配置。
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "server.crt")
	key := filepath.Join(dir, "server.key")
	if err := os.WriteFile(cert, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	data := []byte(`
websocket:
  listen: "127.0.0.1"
  port: 9443
  stream_path: "/v1/stream"
  client_id_header: "X-Hardware-Id"
  tls:
    cert_file: "` + cert + `"
    key_file: "` + key + `"
  rtt_interval: "5s"
  read_timeout: "20s"
  write_timeout: "3s"
  max_message_bytes: 4096
  stream:
    sample_rate: 8000
    channels: 1
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.StreamPath != "/v1/stream" {
		t.Fatalf("StreamPath = %q", cfg.StreamPath)
	}
	if cfg.ClientIDHeader != "X-Hardware-Id" {
		t.Fatalf("ClientIDHeader = %q", cfg.ClientIDHeader)
	}
	if cfg.RTTInterval != 5*time.Second {
		t.Fatalf("RTTInterval = %s", cfg.RTTInterval)
	}
	if cfg.MaxMessageBytes != 4096 {
		t.Fatalf("MaxMessageBytes = %d", cfg.MaxMessageBytes)
	}
	if cfg.Stream.SampleRate != 8000 || cfg.Stream.Channels != 1 {
		t.Fatalf("Stream = %+v", cfg.Stream)
	}
}
