package audioenhancement

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rtc-media-server/internal/log"
	"rtc-media-server/internal/media"
)

const (
	pcmOutputDirPerm  = 0o755
	pcmOutputFilePerm = 0o600
)

// MockStage 模拟 AEC+AGC+ANS 语音增强 stage。
// 当前实现不做真实算法处理，只把经过该 stage 的 PCM 追加保存到本地文件。
type MockStage struct {
	outputDir string
	getRTT    func() (time.Duration, bool)

	mu    sync.Mutex
	files map[string]*os.File
	refs  int
}

// NewMockStage 创建模拟 AEC+AGC+ANS 的 pipeline stage。
// outputDir 用于存放每个 client 独立的 PCM 文件。
func NewMockStage(outputDir string, getRTT func() (time.Duration, bool)) (*MockStage, error) {
	if outputDir == "" {
		outputDir = filepath.Join("runtime", "pcm")
	}
	if err := os.MkdirAll(outputDir, pcmOutputDirPerm); err != nil {
		return nil, fmt.Errorf("create PCM output directory failed: %w", err)
	}
	return &MockStage{
		outputDir: outputDir,
		getRTT:    getRTT,
		files:     make(map[string]*os.File),
	}, nil
}

// Name 返回语音增强 mock stage 名称。
func (s *MockStage) Name() string { return "aec_agc_ans_mock" }

// AddReference 接收下行参考信号。真实 AEC 会把该信号写入回声参考缓冲区。
func (s *MockStage) AddReference(ctx context.Context, frame media.Frame) error {
	s.mu.Lock()
	s.refs++
	s.mu.Unlock()
	log.Infof(
		"client_id=%s audio enhancement reference received direction=%s codec=%s bytes=%d",
		frame.SessionID,
		frame.Direction,
		frame.Format.Codec,
		len(frame.Payload),
	)
	return nil
}

// Process 模拟 AEC+AGC+ANS 处理并透传媒体帧。
func (s *MockStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec == media.CodecPCM16LE {
		if err := s.SavePCM(frame.SessionID, frame.Payload); err != nil {
			return frame, err
		}
		log.Infof(
			"client_id=%s audio enhancement processed direction=%s codec=%s bytes=%d rtt_ms=%s file=%s",
			frame.SessionID,
			frame.Direction,
			frame.Format.Codec,
			len(frame.Payload),
			s.rttMilliseconds(),
			s.filePath(frame.SessionID),
		)
	}
	return frame, nil
}

// SavePCM 把 PCM 数据追加写入该 client 对应的文件。
func (s *MockStage) SavePCM(clientID string, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := s.fileLocked(clientID)
	if err != nil {
		return err
	}
	if _, err := file.Write(pcm); err != nil {
		return fmt.Errorf("write PCM file failed: %w", err)
	}
	return nil
}

// Close 关闭语音增强 stage 持有的所有 PCM 文件。
func (s *MockStage) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var firstErr error
	for clientID, file := range s.files {
		if err := file.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close PCM file failed client=%s: %w", clientID, err)
		}
		delete(s.files, clientID)
	}
	return firstErr
}

// CloseSession 关闭指定 client 对应的 PCM 文件。
func (s *MockStage) CloseSession(clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	file := s.files[clientID]
	if file == nil {
		return nil
	}
	delete(s.files, clientID)
	if err := file.Close(); err != nil {
		return fmt.Errorf("close PCM file failed: %w", err)
	}
	return nil
}

// fileLocked 返回指定 client 的 PCM 输出文件，调用方必须持有 s.mu。
func (s *MockStage) fileLocked(clientID string) (*os.File, error) {
	file := s.files[clientID]
	if file != nil {
		return file, nil
	}

	path := s.filePath(clientID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, pcmOutputFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open PCM file failed: %w", err)
	}
	s.files[clientID] = file
	return file, nil
}

// filePath 返回指定 client 的 PCM 输出文件路径。
func (s *MockStage) filePath(clientID string) string {
	return filepath.Join(s.outputDir, safeFileName(clientID)+".pcm")
}

// rttMilliseconds 返回当前 Session 最近一次 RTT 毫秒数。
func (s *MockStage) rttMilliseconds() string {
	if s.getRTT == nil {
		return ""
	}
	rtt, ok := s.getRTT()
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d", rtt.Milliseconds())
}

// safeFileName 将客户端标识转换为可用作文件名的安全字符串。
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
