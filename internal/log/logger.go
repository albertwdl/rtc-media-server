// Package log 提供项目统一的格式化日志入口。
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var (
	mu sync.RWMutex

	logger     *slog.Logger
	logFile    *os.File
	rootDir    = projectRoot()
	packageDir = packageRoot()
)

// Options 定义日志模块初始化所需的最小配置。
type Options struct {
	Level  string
	Format string
	File   string
}

// Init 根据日志配置初始化全局日志实例。
func Init(opts Options) error {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return err
	}

	writer, err := openLogFile(opts.File)
	if err != nil {
		return err
	}

	handler, err := newHandler(writer, level, opts.Format)
	if err != nil {
		_ = writer.Close()
		return err
	}

	nextLogger := slog.New(handler)

	mu.Lock()
	defer mu.Unlock()

	if logFile != nil {
		_ = logFile.Close()
	}
	logger = nextLogger
	logFile = writer
	return nil
}

// Debugf 输出调试日志。
func Debugf(format string, args ...any) {
	logf(slog.LevelDebug, format, args...)
}

// Infof 输出普通信息日志。
func Infof(format string, args ...any) {
	logf(slog.LevelInfo, format, args...)
}

// Warnf 输出警告日志。
func Warnf(format string, args ...any) {
	logf(slog.LevelWarn, format, args...)
}

// Errorf 输出错误日志。
func Errorf(format string, args ...any) {
	logf(slog.LevelError, format, args...)
}

func logf(level slog.Level, format string, args ...any) {
	current := mustLogger()
	current.LogAttrs(
		context.Background(),
		level,
		fmt.Sprintf(format, args...),
		slog.String("source", caller()),
	)
}

func mustLogger() *slog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	if logger == nil {
		panic("log not initialized")
	}
	return logger
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q", raw)
	}
}

func newHandler(writer *os.File, level slog.Level, format string) (slog.Handler, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text":
		return newBracketTextHandler(writer, level), nil
	case "json":
		opts := &slog.HandlerOptions{Level: level, AddSource: false}
		return slog.NewJSONHandler(writer, opts), nil
	default:
		return nil, fmt.Errorf("invalid log format %q", format)
	}
}

func openLogFile(path string) (*os.File, error) {
	clean := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(clean, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

func caller() string {
	pcs := make([]uintptr, 16)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.File == "" {
			if !more {
				break
			}
			continue
		}
		if !sameDir(filepath.Dir(frame.File), packageDir) || strings.HasSuffix(frame.File, "_test.go") {
			return formatCaller(frame.File, frame.Line)
		}
		if !more {
			break
		}
	}
	return "unknown:0"
}

func formatCaller(file string, line int) string {
	rel, err := filepath.Rel(rootDir, file)
	if err != nil {
		return fmt.Sprintf("%s:%d", filepath.ToSlash(file), line)
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), line)
}

func packageRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Dir(file)
}

func projectRoot() string {
	dir := packageRoot()
	if dir == "" {
		return ""
	}
	return filepath.Clean(filepath.Join(dir, "..", ".."))
}

func sameDir(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

type bracketTextHandler struct {
	writer io.Writer
	level  slog.Level
	attrs  []slog.Attr
	mu     *sync.Mutex
}

func newBracketTextHandler(writer io.Writer, level slog.Level) slog.Handler {
	return &bracketTextHandler{
		writer: writer,
		level:  level,
		mu:     &sync.Mutex{},
	}
}

func (h *bracketTextHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *bracketTextHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := make([]slog.Attr, 0, len(h.attrs)+4)
	attrs = append(attrs, h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})

	source := ""
	extra := make([]string, 0, len(attrs))
	for _, attr := range attrs {
		attr.Value = attr.Value.Resolve()
		if attr.Equal(slog.Attr{}) {
			continue
		}
		if attr.Key == "source" {
			source = valueString(attr.Value)
			continue
		}
		extra = append(extra, fmt.Sprintf("%s=%s", attr.Key, valueString(attr.Value)))
	}

	var builder strings.Builder
	builder.Grow(len(record.Message) + len(source) + 64)
	builder.WriteString("[")
	builder.WriteString(record.Time.Format(timeFormat))
	builder.WriteString("] [")
	builder.WriteString(strings.ToUpper(record.Level.String()))
	builder.WriteString("] [")
	if source == "" {
		builder.WriteString("unknown:0")
	} else {
		builder.WriteString(source)
	}
	builder.WriteString("] ")
	builder.WriteString(record.Message)
	if len(extra) > 0 {
		builder.WriteString(" ")
		builder.WriteString(strings.Join(extra, " "))
	}
	builder.WriteString("\n")

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.writer, builder.String())
	return err
}

func (h *bracketTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	combined := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	combined = append(combined, h.attrs...)
	combined = append(combined, attrs...)
	return &bracketTextHandler{
		writer: h.writer,
		level:  h.level,
		attrs:  combined,
		mu:     h.mu,
	}
}

func (h *bracketTextHandler) WithGroup(_ string) slog.Handler {
	return h
}

func valueString(value slog.Value) string {
	return fmt.Sprint(value.Any())
}

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

func resetForTest() {
	mu.Lock()
	defer mu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
	}
	logger = nil
	logFile = nil
}
