package audioenhancement

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"rtc-media-server/internal/media"
)

// MockEngine 模拟 AEC+AGC+ANS 语音增强引擎。
// 当前实现不做真实算法处理，只把经过该 stage 的 PCM 追加保存到本地文件。
type MockEngine struct {
	outputDir string
	logger    *slog.Logger

	mu    sync.Mutex
	files map[string]*os.File
}

// NewMockEngine 创建一个模拟 AEC+AGC+ANS 语音增强引擎。
// outputDir 用于存放每个 client 独立的 PCM 文件。
func NewMockEngine(outputDir string, logger *slog.Logger) (*MockEngine, error) {
	if outputDir == "" {
		outputDir = filepath.Join("runtime", "pcm")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 PCM 输出目录失败: %w", err)
	}
	return &MockEngine{
		outputDir: outputDir,
		logger:    logger,
		files:     make(map[string]*os.File),
	}, nil
}

func (e *MockEngine) Name() string { return "aec_agc_ans_mock" }

// Process 实现 media.Stage，模拟 AEC+AGC+ANS 处理并透传媒体帧。
func (e *MockEngine) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec == media.CodecPCM16LE {
		if err := e.SavePCM(frame.SessionID, frame.Payload); err != nil {
			return frame, err
		}
		e.logger.Info(
			"client_id="+frame.SessionID+" audio enhancement processed",
			slog.String("client_id", frame.SessionID),
			slog.String("direction", string(frame.Direction)),
			slog.String("codec", frame.Format.Codec),
			slog.Int("bytes", len(frame.Payload)),
			slog.String("file", e.filePath(frame.SessionID)),
		)
	}
	return frame, nil
}

// SavePCM 把 PCM 数据追加写入该 client 对应的文件。
func (e *MockEngine) SavePCM(clientID string, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	file, err := e.fileLocked(clientID)
	if err != nil {
		return err
	}
	if _, err := file.Write(pcm); err != nil {
		return fmt.Errorf("写入 PCM 文件失败: %w", err)
	}
	return nil
}

// Close 关闭语音增强引擎持有的所有 PCM 文件。
func (e *MockEngine) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var firstErr error
	for clientID, file := range e.files {
		if err := file.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("关闭 PCM 文件失败 client=%s: %w", clientID, err)
		}
		delete(e.files, clientID)
	}
	return firstErr
}

// CloseSession 关闭指定 client 对应的 PCM 文件。
func (e *MockEngine) CloseSession(clientID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	file := e.files[clientID]
	if file == nil {
		return nil
	}
	delete(e.files, clientID)
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭 PCM 文件失败: %w", err)
	}
	return nil
}

func (e *MockEngine) fileLocked(clientID string) (*os.File, error) {
	file := e.files[clientID]
	if file != nil {
		return file, nil
	}

	path := e.filePath(clientID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 PCM 文件失败: %w", err)
	}
	e.files[clientID] = file
	return file, nil
}

func (e *MockEngine) filePath(clientID string) string {
	return filepath.Join(e.outputDir, safeFileName(clientID)+".pcm")
}

func safeFileName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}

	var builder strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			builder.WriteRune(r)
			continue
		}
		builder.WriteByte('_')
	}
	return builder.String()
}
