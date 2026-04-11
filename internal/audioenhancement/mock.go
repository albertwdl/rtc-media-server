package audioenhancement

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"rtc-media-server/internal/websocket"
)

// MockEngine 模拟 AEC+AGC+ANS 语音增强引擎。
// 当前实现不做真实算法处理，只把 websocket 回调得到的 PCM 追加保存到本地文件。
type MockEngine struct {
	outputDir string

	mu    sync.Mutex
	files map[string]*os.File
}

// NewMockEngine 创建一个模拟 AEC+AGC+ANS 语音增强引擎。
// outputDir 用于存放每个 client 独立的 PCM 文件。
func NewMockEngine(outputDir string) (*MockEngine, error) {
	if outputDir == "" {
		outputDir = filepath.Join("runtime", "pcm")
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 PCM 输出目录失败: %w", err)
	}
	return &MockEngine{
		outputDir: outputDir,
		files:     make(map[string]*os.File),
	}, nil
}

// BindSession 把某个 websocket Session 的回调注册到语音增强引擎。
func (e *MockEngine) BindSession(session *websocket.Session) {
	log.Printf("语音增强模拟引擎绑定客户端: %s", session.ID())
	session.SetCallbacks(websocket.SessionCallbacks{
		OnStreamEvent: func(session *websocket.Session, event websocket.StreamEvent) {
			log.Printf("stream事件 client=%s type=%s payload_bytes=%d", session.ID(), event.Type, len(event.RawJSON))
		},
		OnStreamPCM: func(session *websocket.Session, pcm []byte) {
			if err := e.SavePCM(session.ID(), pcm); err != nil {
				log.Printf("保存PCM失败 client=%s err=%v", session.ID(), err)
				return
			}
			log.Printf("已保存PCM client=%s bytes=%d file=%s", session.ID(), len(pcm), e.filePath(session.ID()))
		},
		OnCommand: func(session *websocket.Session, payload []byte) {
			log.Printf("cmd消息 client=%s payload=%s", session.ID(), string(payload))
		},
		OnDisconnected: func(session *websocket.Session, err error) {
			log.Printf("客户端已断开: %s err=%v", session.ID(), err)
			if closeErr := e.CloseSession(session.ID()); closeErr != nil {
				log.Printf("关闭PCM文件失败 client=%s err=%v", session.ID(), closeErr)
			}
		},
		OnError: func(session *websocket.Session, err error) {
			log.Printf("客户端错误: %s err=%v", session.ID(), err)
		},
	})
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
func (e *MockEngine) Close() error {
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
