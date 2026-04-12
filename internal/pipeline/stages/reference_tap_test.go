package stages

import (
	"context"
	"testing"

	"rtc-media-server/internal/media"
)

// TestReferenceTapCopiesPCMFrame 验证 reference tap 会复制 PCM 参考帧。
func TestReferenceTapCopiesPCMFrame(t *testing.T) {
	refCh := make(chan media.Frame, 1)
	stage := NewReferenceTap(func(ctx context.Context, frame media.Frame) {
		refCh <- frame
	})
	frame := media.Frame{
		SessionID: "client-a",
		Direction: media.DirectionDownlink,
		Payload:   []byte{0x01, 0x02},
		Format:    media.DefaultPCM16Format(),
	}

	got, err := stage.Process(context.Background(), frame)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if string(got.Payload) != string(frame.Payload) {
		t.Fatalf("output payload = %#v", got.Payload)
	}
	ref := <-refCh
	if string(ref.Payload) != string(frame.Payload) {
		t.Fatalf("reference payload = %#v", ref.Payload)
	}
	ref.Payload[0] = 0xFF
	if got.Payload[0] == 0xFF {
		t.Fatal("reference payload shares backing array with output")
	}
}
