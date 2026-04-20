// Package config provides process-wide application configuration loading.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultServerHost     = "0.0.0.0"
	DefaultServerPort     = 8161
	DefaultStreamPath     = "/v1/realtime"
	DefaultClientIDHeader = "X-Hardware-Id"
	DefaultListen         = "0.0.0.0"
	DefaultPort           = 8443
	DefaultRTTInterval    = 10 * time.Second
	DefaultReadTimeout    = 30 * time.Second
	DefaultWriteTimeout   = 10 * time.Second
	DefaultMaxMessageSize = 1 << 20
	MaxTCPPort            = 65535
	DefaultSampleRate     = 8000
	DefaultChannels       = 1
	DefaultCloseTimeout   = 3 * time.Second
	DefaultInitialSilence = 15 * time.Second
	DefaultSilenceTimeout = 5 * time.Second
	DefaultReferenceQueue = 16
	DefaultSignalingURL   = "http://10.85.216.139:18084"
	DefaultWebRTCRoomID   = "005"
	DefaultWebRTCToken    = "12345"
	DefaultWebRTCPortMin  = 10000
	DefaultWebRTCPortMax  = 11000
	DefaultAgentProtocol  = "wss"
	DefaultAgentHost      = "localhost"
	DefaultAgentPort      = 8160
	DefaultAgentPath      = "/v1/ai-edge-agent/omni/session"
	DefaultTTSSampleRate  = 24000

	defaultConfigFileYAML = "config.yaml"
	defaultConfigFileYML  = "config.yml"
	defaultConfigDir      = "configs"
	defaultLogLevel       = "info"
	defaultLogFormat      = "text"
	defaultLogFile        = "logs/rtc-media-server.log"
	defaultTLSCertFile    = "certs/server.crt"
	defaultTLSKeyFile     = "certs/server.key"
	defaultTLSClientCA    = "certs/ca.crt"
	defaultAgentCipher    = "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"
)

// Duration stores a time.Duration while accepting Go duration strings in YAML.
type Duration time.Duration

// Duration returns the value as time.Duration.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// String returns the Go duration string form.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// UnmarshalYAML parses a Go duration string, for example "10s" or "3s".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var text string
	if err := value.Decode(&text); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML writes the value as a Go duration string.
func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

// Config is the complete application configuration loaded from YAML.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Log        LogConfig        `yaml:"log"`
	Signaling  SignalingConfig  `yaml:"signaling"`
	WebRTC     WebRTCConfig     `yaml:"webrtc"`
	TLS        TLSConfig        `yaml:"tls"`
	Agent      AgentConfig      `yaml:"agent"`
	WebSocket  WebSocketConfig  `yaml:"websocket"`
	Session    SessionConfig    `yaml:"session"`
	Controller ControllerConfig `yaml:"controller"`
}

// ServerConfig stores HTTP service listener settings.
type ServerConfig struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
}

// SignalingConfig stores signaling service connection settings.
type SignalingConfig struct {
	URL                    string `yaml:"url"`
	ReconnectionAttempts   int    `yaml:"reconnection_attempts"`
	ReconnectionIntervalMS int    `yaml:"reconnection_interval_ms"`
}

// WebSocketConfig defines WebSocket listener, TLS, timeout, RTT, and stream settings.
type WebSocketConfig struct {
	Listen          string             `yaml:"listen"`
	Port            int                `yaml:"port"`
	StreamPath      string             `yaml:"stream_path"`
	ClientIDHeader  string             `yaml:"client_id_header"`
	TLS             WebSocketTLSConfig `yaml:"tls"`
	RTTInterval     Duration           `yaml:"rtt_interval"`
	ReadTimeout     Duration           `yaml:"read_timeout"`
	WriteTimeout    Duration           `yaml:"write_timeout"`
	MaxMessageBytes int64              `yaml:"max_message_bytes"`
	Stream          StreamConfig       `yaml:"stream"`
}

// WebSocketTLSConfig defines certificate and private key paths required for WSS.
type WebSocketTLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// TLSConfig stores top-level TLS settings.
type TLSConfig struct {
	Enabled      bool   `yaml:"enabled"`
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// StreamConfig defines the current stream audio format.
type StreamConfig struct {
	SampleRate int `yaml:"sample_rate"`
	Channels   int `yaml:"channels"`
}

// SessionConfig stores session-level runtime settings from YAML.
type SessionConfig struct {
	CloseTimeout Duration `yaml:"close_timeout"`
	IdleTimeout  Duration `yaml:"idle_timeout"`
}

// ControllerConfig stores cross-pipeline controller settings from YAML.
type ControllerConfig struct {
	InitialSilenceTimeout Duration `yaml:"initial_silence_timeout"`
	SilenceTimeout        Duration `yaml:"silence_timeout"`
	ReferenceQueueSize    int      `yaml:"reference_queue_size"`
}

// LogConfig stores logger configuration from YAML.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	File   string `yaml:"file"`
}

// WebRTCConfig stores future WebRTC feature flags from YAML.
type WebRTCConfig struct {
	Enabled bool   `yaml:"enabled"`
	RoomID  string `yaml:"room_id"`
	Token   string `yaml:"token"`
	PortMin int    `yaml:"port_min"`
	PortMax int    `yaml:"port_max"`
}

// AgentConfig stores AI edge agent connection settings.
type AgentConfig struct {
	Protocol      string `yaml:"protocol"`
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	Path          string `yaml:"path"`
	SkipVerify    bool   `yaml:"skip_verify"`
	CACertFile    string `yaml:"ca_cert_file"`
	CipherSuites  string `yaml:"cipher_suites"`
	MinVersion    string `yaml:"min_version"`
	ImageTimeout  int    `yaml:"image_timeout"`
	TTSSampleRate int    `yaml:"tts_sample_rate"`
}

var (
	mu     sync.RWMutex
	loaded bool
	active Config
)

// Load reads YAML configuration, validates it, and installs it as the process singleton.
func Load(path string) (*Config, error) {
	mu.Lock()
	defer mu.Unlock()
	if loaded {
		return nil, errors.New("config already loaded")
	}

	cfg, err := loadFile(path)
	if err != nil {
		return nil, err
	}
	active = cfg
	loaded = true

	copy := active
	return &copy, nil
}

// Get returns a copy of the loaded process configuration.
func Get() Config {
	mu.RLock()
	defer mu.RUnlock()
	if !loaded {
		panic("config not loaded")
	}
	return active
}

// ResetForTest clears the process singleton so config tests can load fresh files.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	active = Config{}
	loaded = false
}

// DefaultConfig returns the default complete application configuration.
func DefaultConfig() Config {
	controllerCfg := DefaultControllerConfig()
	return Config{
		Server:     DefaultServerConfig(),
		Log:        DefaultLogConfig(),
		Signaling:  DefaultSignalingConfig(),
		WebRTC:     DefaultWebRTCConfig(),
		TLS:        DefaultTLSConfig(),
		Agent:      DefaultAgentConfig(),
		WebSocket:  DefaultWebSocketConfig(),
		Session:    DefaultSessionConfig(),
		Controller: controllerCfg,
	}
}

// DefaultServerConfig returns the default HTTP service configuration.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Host: DefaultServerHost,
		Port: DefaultServerPort,
	}
}

// DefaultLogConfig returns the default logger configuration.
func DefaultLogConfig() LogConfig {
	return LogConfig{
		Level:  defaultLogLevel,
		Format: defaultLogFormat,
		File:   defaultLogFile,
	}
}

// DefaultSignalingConfig returns the default signaling service configuration.
func DefaultSignalingConfig() SignalingConfig {
	return SignalingConfig{
		URL:                    DefaultSignalingURL,
		ReconnectionAttempts:   5,
		ReconnectionIntervalMS: 3000,
	}
}

// DefaultTLSConfig returns the default top-level TLS configuration.
func DefaultTLSConfig() TLSConfig {
	return TLSConfig{
		Enabled:      true,
		CertFile:     defaultTLSCertFile,
		KeyFile:      defaultTLSKeyFile,
		ClientCAFile: defaultTLSClientCA,
	}
}

// DefaultWebRTCConfig returns the default WebRTC configuration.
func DefaultWebRTCConfig() WebRTCConfig {
	return WebRTCConfig{
		Enabled: false,
		RoomID:  DefaultWebRTCRoomID,
		Token:   DefaultWebRTCToken,
		PortMin: DefaultWebRTCPortMin,
		PortMax: DefaultWebRTCPortMax,
	}
}

// DefaultAgentConfig returns the default agent connection configuration.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Protocol:      DefaultAgentProtocol,
		Host:          DefaultAgentHost,
		Port:          DefaultAgentPort,
		Path:          DefaultAgentPath,
		SkipVerify:    true,
		CipherSuites:  defaultAgentCipher,
		TTSSampleRate: DefaultTTSSampleRate,
		ImageTimeout:  10,
	}
}

// DefaultWebSocketConfig returns the default WebSocket runtime configuration.
func DefaultWebSocketConfig() WebSocketConfig {
	return WebSocketConfig{
		Listen:          DefaultListen,
		Port:            DefaultPort,
		StreamPath:      DefaultStreamPath,
		ClientIDHeader:  DefaultClientIDHeader,
		RTTInterval:     Duration(DefaultRTTInterval),
		ReadTimeout:     Duration(DefaultReadTimeout),
		WriteTimeout:    Duration(DefaultWriteTimeout),
		MaxMessageBytes: DefaultMaxMessageSize,
		Stream: StreamConfig{
			SampleRate: DefaultSampleRate,
			Channels:   DefaultChannels,
		},
	}
}

// DefaultSessionConfig returns the default session configuration.
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		CloseTimeout: Duration(DefaultCloseTimeout),
		IdleTimeout:  0,
	}
}

// DefaultControllerConfig returns the default controller configuration.
func DefaultControllerConfig() ControllerConfig {
	return ControllerConfig{
		InitialSilenceTimeout: Duration(DefaultInitialSilence),
		SilenceTimeout:        Duration(DefaultSilenceTimeout),
		ReferenceQueueSize:    DefaultReferenceQueue,
	}
}

// ValidateWebSocketConfig checks required WebSocket config fields and ranges.
func ValidateWebSocketConfig(cfg WebSocketConfig) error {
	if cfg.Port <= 0 || cfg.Port > MaxTCPPort {
		return fmt.Errorf("invalid websocket port %d", cfg.Port)
	}
	if cfg.StreamPath == "" || cfg.StreamPath[0] != '/' {
		return fmt.Errorf("invalid websocket stream path %q", cfg.StreamPath)
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
	if cfg.ReadTimeout.Duration() <= 0 {
		return errors.New("websocket read_timeout must be positive")
	}
	if cfg.WriteTimeout.Duration() <= 0 {
		return errors.New("websocket write_timeout must be positive")
	}
	if cfg.RTTInterval.Duration() <= 0 {
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

// ValidateWebSocketTLSFiles checks that configured TLS files are accessible.
func ValidateWebSocketTLSFiles(cfg WebSocketConfig) error {
	if _, err := os.Stat(cfg.TLS.CertFile); err != nil {
		return fmt.Errorf("stat websocket tls cert_file %q: %w", cfg.TLS.CertFile, err)
	}
	if _, err := os.Stat(cfg.TLS.KeyFile); err != nil {
		return fmt.Errorf("stat websocket tls key_file %q: %w", cfg.TLS.KeyFile, err)
	}
	return nil
}

func loadFile(path string) (Config, error) {
	if path == "" {
		var err error
		path, err = defaultConfigPath()
		if err != nil {
			return Config{}, err
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaultConfigPath() (string, error) {
	for _, name := range []string{defaultConfigFileYAML, defaultConfigFileYML} {
		path := filepath.Join(defaultConfigDir, name)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", errors.New("config not found: tried configs/config.yaml and configs/config.yml")
}

func validateConfig(cfg Config) error {
	if cfg.Server.Port <= 0 || cfg.Server.Port > MaxTCPPort {
		return fmt.Errorf("invalid server port %d", cfg.Server.Port)
	}
	if cfg.Signaling.URL == "" {
		return errors.New("signaling.url is required")
	}
	if cfg.WebRTC.PortMin <= 0 || cfg.WebRTC.PortMin > MaxTCPPort {
		return fmt.Errorf("invalid webrtc port_min %d", cfg.WebRTC.PortMin)
	}
	if cfg.WebRTC.PortMax <= 0 || cfg.WebRTC.PortMax > MaxTCPPort {
		return fmt.Errorf("invalid webrtc port_max %d", cfg.WebRTC.PortMax)
	}
	if cfg.WebRTC.PortMin > cfg.WebRTC.PortMax {
		return fmt.Errorf("invalid webrtc port range %d-%d", cfg.WebRTC.PortMin, cfg.WebRTC.PortMax)
	}
	if cfg.TLS.Enabled {
		if cfg.TLS.CertFile == "" {
			return errors.New("tls.cert_file is required when tls.enabled is true")
		}
		if cfg.TLS.KeyFile == "" {
			return errors.New("tls.key_file is required when tls.enabled is true")
		}
	}
	if cfg.Agent.Protocol == "" {
		return errors.New("agent.protocol is required")
	}
	if cfg.Agent.Host == "" {
		return errors.New("agent.host is required")
	}
	if cfg.Agent.Port <= 0 || cfg.Agent.Port > MaxTCPPort {
		return fmt.Errorf("invalid agent port %d", cfg.Agent.Port)
	}
	if cfg.Agent.Path == "" {
		return errors.New("agent.path is required")
	}
	if cfg.Agent.TTSSampleRate <= 0 {
		return errors.New("agent.tts_sample_rate must be positive")
	}
	if err := ValidateWebSocketConfig(cfg.WebSocket); err != nil {
		return err
	}
	if cfg.Session.CloseTimeout.Duration() <= 0 {
		return errors.New("session.close_timeout must be positive")
	}
	if cfg.Session.IdleTimeout.Duration() < 0 {
		return errors.New("session.idle_timeout must not be negative")
	}
	if cfg.Controller.InitialSilenceTimeout.Duration() <= 0 {
		return errors.New("controller.initial_silence_timeout must be positive")
	}
	if cfg.Controller.SilenceTimeout.Duration() <= 0 {
		return errors.New("controller.silence_timeout must be positive")
	}
	if cfg.Controller.ReferenceQueueSize <= 0 {
		return errors.New("controller.reference_queue_size must be positive")
	}
	return nil
}
