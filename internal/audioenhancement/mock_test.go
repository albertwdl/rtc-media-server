package audioenhancement

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMockEngineSavePCM 验证语音增强 mock 会保存 PCM 数据。
func TestMockEngineSavePCM(t *testing.T) {
	dir := t.TempDir()
	engine, err := NewMockEngine(dir, nil)
	if err != nil {
		t.Fatalf("NewMockEngine: %v", err)
	}
	defer engine.Close(context.Background())

	if err := engine.SavePCM("client/a", []byte{0x01, 0x02}); err != nil {
		t.Fatalf("SavePCM first: %v", err)
	}
	if err := engine.SavePCM("client/a", []byte{0x03}); err != nil {
		t.Fatalf("SavePCM second: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "client_a.pcm"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := []byte{0x01, 0x02, 0x03}
	if string(got) != string(want) {
		t.Fatalf("pcm = %#v, want %#v", got, want)
	}
}
