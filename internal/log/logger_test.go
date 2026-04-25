package log

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInitTextWritesAllLevels(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	path := filepath.Join(t.TempDir(), "logs", "app.log")
	if err := Init(Options{Level: "debug", Format: "text", File: path}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	Debugf("debug message")
	Infof("info message")
	Warnf("warn message")
	Errorf("error message")

	content := readLogFile(t, path)
	for _, want := range []string{"debug message", "info message", "warn message", "error message"} {
		if !strings.Contains(content, want) {
			t.Fatalf("log output missing %q: %s", want, content)
		}
	}
	if strings.Contains(content, "time=") || strings.Contains(content, "level=") || strings.Contains(content, "caller=") {
		t.Fatalf("text log should use bracket format: %s", content)
	}
}

func TestInfoLevelFiltersDebug(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	path := filepath.Join(t.TempDir(), "level.log")
	if err := Init(Options{Level: "info", Format: "text", File: path}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	Debugf("hidden debug")
	Infof("visible info")

	content := readLogFile(t, path)
	if strings.Contains(content, "hidden debug") {
		t.Fatalf("debug log should be filtered: %s", content)
	}
	if !strings.Contains(content, "visible info") {
		t.Fatalf("info log missing: %s", content)
	}
}

func TestCallerPointsToRealCallSite(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	path := filepath.Join(t.TempDir(), "caller.log")
	if err := Init(Options{Level: "info", Format: "text", File: path}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	line := currentLine() + 1
	Infof("caller check")

	content := readLogFile(t, path)
	want := fmt.Sprintf("[INFO] [internal/log/logger_test.go:%d] caller check", line)
	if !strings.Contains(content, want) {
		t.Fatalf("source = %q, want substring %q", content, want)
	}
	if strings.Contains(content, "internal/log/logger.go") {
		t.Fatalf("source should not point to logger.go: %s", content)
	}
	if !strings.HasPrefix(content, "[") {
		t.Fatalf("text log should start with bracket prefix: %s", content)
	}
}

func TestInitJSONWritesCaller(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	path := filepath.Join(t.TempDir(), "json.log")
	if err := Init(Options{Level: "info", Format: "json", File: path}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	Infof("json message")

	content := readLogFile(t, path)
	if !strings.Contains(content, `"msg":"json message"`) {
		t.Fatalf("json message missing: %s", content)
	}
	if !strings.Contains(content, `"source":"internal/log/logger_test.go:`) {
		t.Fatalf("json source missing: %s", content)
	}
	if strings.Contains(content, `"caller":`) {
		t.Fatalf("json should not contain caller: %s", content)
	}
}

func TestInitRejectsInvalidConfig(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	path := filepath.Join(t.TempDir(), "invalid.log")
	if err := Init(Options{Level: "bad", Format: "text", File: path}); err == nil {
		t.Fatal("Init should reject invalid level")
	}
	if err := Init(Options{Level: "info", Format: "bad", File: path}); err == nil {
		t.Fatal("Init should reject invalid format")
	}
}

func TestInitCreatesParentDirectory(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	path := filepath.Join(t.TempDir(), "nested", "logs", "app.log")
	if err := Init(Options{Level: "info", Format: "text", File: path}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	Infof("created directory")

	content := readLogFile(t, path)
	if !strings.Contains(content, "created directory") {
		t.Fatalf("log output missing: %s", content)
	}
}

func TestLogWithoutInitPanics(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	defer func() {
		if recover() == nil {
			t.Fatal("Infof should panic before Init")
		}
	}()

	Infof("should panic")
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(data)
}

func currentLine() int {
	_, _, line, ok := runtime.Caller(1)
	if !ok {
		return 0
	}
	return line
}
