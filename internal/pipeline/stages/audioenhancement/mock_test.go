package audioenhancement

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMockStageSavePCM 验证语音增强 mock stage 会保存 PCM 数据。
func TestMockStageSavePCM(t *testing.T) {
	dir := t.TempDir()
	stage, err := NewMockStage(dir)
	if err != nil {
		t.Fatalf("NewMockStage: %v", err)
	}
	defer stage.Close(context.Background())

	if err := stage.SavePCM("client/a", []byte{0x01, 0x02}); err != nil {
		t.Fatalf("SavePCM first: %v", err)
	}
	if err := stage.SavePCM("client/a", []byte{0x03}); err != nil {
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
