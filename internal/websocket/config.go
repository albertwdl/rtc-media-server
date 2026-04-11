package websocket

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultStreamPath     = "/v1/stream"
	DefaultCmdPath        = "/v1/cmd"
	DefaultClientIDHeader = "X-Hardware-Id"
)

// Config 定义 WebSocket 服务端监听、TLS、超时、RTT 和 stream 载荷配置。
type Config struct {
	Listen          string
	Port            int
	StreamPath      string
	CmdPath         string
	ClientIDHeader  string
	TLS             TLSConfig
	RTTInterval     time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	MaxMessageBytes int64
	Stream          StreamConfig
}

// TLSConfig 定义启用 WSS 所需的证书和私钥文件路径。
type TLSConfig struct {
	CertFile string
	KeyFile  string
}

// StreamConfig 定义 stream 通道当前承载的原始 PCM 音频格式。
type StreamConfig struct {
	SampleRate int
	Channels   int
}

// DefaultConfig 返回模块的默认配置。
func DefaultConfig() Config {
	return Config{
		Listen:          "0.0.0.0",
		Port:            8443,
		StreamPath:      DefaultStreamPath,
		CmdPath:         DefaultCmdPath,
		ClientIDHeader:  DefaultClientIDHeader,
		RTTInterval:     10 * time.Second,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    10 * time.Second,
		MaxMessageBytes: 1 << 20,
		Stream: StreamConfig{
			SampleRate: 8000,
			Channels:   1,
		},
	}
}

// LoadConfig 从 YAML 文件加载 websocket 配置。
// path 为空时会依次尝试 configs/config.yaml 和 configs/config.yml。
func LoadConfig(path string) (Config, error) {
	if path == "" {
		var err error
		path, err = defaultConfigPath()
		if err != nil {
			return Config{}, err
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read websocket config %q: %w", path, err)
	}

	raw := rootConfig{WebSocket: defaultRawConfig()}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse websocket config %q: %w", path, err)
	}

	cfg, err := raw.WebSocket.config()
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaultConfigPath() (string, error) {
	for _, path := range []string{
		filepath.Join("configs", "config.yaml"),
		filepath.Join("configs", "config.yml"),
	} {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", errors.New("websocket config not found: tried configs/config.yaml and configs/config.yml")
}

func validateConfig(cfg Config) error {
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return fmt.Errorf("invalid websocket port %d", cfg.Port)
	}
	if cfg.StreamPath == "" || cfg.StreamPath[0] != '/' {
		return fmt.Errorf("invalid websocket stream path %q", cfg.StreamPath)
	}
	if cfg.CmdPath == "" || cfg.CmdPath[0] != '/' {
		return fmt.Errorf("invalid websocket cmd path %q", cfg.CmdPath)
	}
	if cfg.StreamPath == cfg.CmdPath {
		return fmt.Errorf("stream path and cmd path must be different: %q", cfg.StreamPath)
	}
	if cfg.ClientIDHeader == "" {
		return errors.New("websocket client id header is required")
	}
	if cfg.TLS.CertFile == "" {
		return errors.New("websocket tls cert_file is required")
	}
	if cfg.TLS.KeyFile == "" {
		return errors.New("websocket tls key_file is required")
	}
	if cfg.ReadTimeout <= 0 {
		return errors.New("websocket read_timeout must be positive")
	}
	if cfg.WriteTimeout <= 0 {
		return errors.New("websocket write_timeout must be positive")
	}
	if cfg.RTTInterval <= 0 {
		return errors.New("websocket rtt_interval must be positive")
	}
	if cfg.MaxMessageBytes <= 0 {
		return errors.New("websocket max_message_bytes must be positive")
	}
	if cfg.Stream.SampleRate <= 0 {
		return errors.New("websocket stream sample_rate must be positive")
	}
	if cfg.Stream.Channels <= 0 {
		return errors.New("websocket stream channels must be positive")
	}
	return nil
}

func validateTLSFiles(cfg Config) error {
	if _, err := os.Stat(cfg.TLS.CertFile); err != nil {
		return fmt.Errorf("stat websocket tls cert_file %q: %w", cfg.TLS.CertFile, err)
	}
	if _, err := os.Stat(cfg.TLS.KeyFile); err != nil {
		return fmt.Errorf("stat websocket tls key_file %q: %w", cfg.TLS.KeyFile, err)
	}
	return nil
}

type rootConfig struct {
	WebSocket rawConfig `yaml:"websocket"`
}

type rawConfig struct {
	Listen          string          `yaml:"listen"`
	Port            int             `yaml:"port"`
	StreamPath      string          `yaml:"stream_path"`
	CmdPath         string          `yaml:"cmd_path"`
	ClientIDHeader  string          `yaml:"client_id_header"`
	TLS             rawTLSConfig    `yaml:"tls"`
	RTTInterval     string          `yaml:"rtt_interval"`
	ReadTimeout     string          `yaml:"read_timeout"`
	WriteTimeout    string          `yaml:"write_timeout"`
	MaxMessageBytes int64           `yaml:"max_message_bytes"`
	Stream          rawStreamConfig `yaml:"stream"`
}

type rawTLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type rawStreamConfig struct {
	SampleRate int `yaml:"sample_rate"`
	Channels   int `yaml:"channels"`
}

func defaultRawConfig() rawConfig {
	cfg := DefaultConfig()
	return rawConfig{
		Listen:          cfg.Listen,
		Port:            cfg.Port,
		StreamPath:      cfg.StreamPath,
		CmdPath:         cfg.CmdPath,
		ClientIDHeader:  cfg.ClientIDHeader,
		RTTInterval:     cfg.RTTInterval.String(),
		ReadTimeout:     cfg.ReadTimeout.String(),
		WriteTimeout:    cfg.WriteTimeout.String(),
		MaxMessageBytes: cfg.MaxMessageBytes,
		Stream: rawStreamConfig{
			SampleRate: cfg.Stream.SampleRate,
			Channels:   cfg.Stream.Channels,
		},
	}
}

func (raw rawConfig) config() (Config, error) {
	rttInterval, err := parseDuration("rtt_interval", raw.RTTInterval)
	if err != nil {
		return Config{}, err
	}
	readTimeout, err := parseDuration("read_timeout", raw.ReadTimeout)
	if err != nil {
		return Config{}, err
	}
	writeTimeout, err := parseDuration("write_timeout", raw.WriteTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen:          raw.Listen,
		Port:            raw.Port,
		StreamPath:      raw.StreamPath,
		CmdPath:         raw.CmdPath,
		ClientIDHeader:  raw.ClientIDHeader,
		TLS:             TLSConfig(raw.TLS),
		RTTInterval:     rttInterval,
		ReadTimeout:     readTimeout,
		WriteTimeout:    writeTimeout,
		MaxMessageBytes: raw.MaxMessageBytes,
		Stream: StreamConfig{
			SampleRate: raw.Stream.SampleRate,
			Channels:   raw.Stream.Channels,
		},
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid websocket %s %q: %w", name, value, err)
	}
	return d, nil
}
