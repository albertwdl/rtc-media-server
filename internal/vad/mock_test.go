package vad

import (
	"context"
	"testing"
	"time"

	"rtc-media-server/internal/controller"
	"rtc-media-server/internal/media"
)

func TestMockStageProcessPassesFrameThrough(t *testing.T) {
	stage := NewMockStage(nil)
	frame := media.Frame{
		SessionID: "client-a",
		Direction: media.DirectionUplink,
		Payload:   []byte{0x01, 0x02},
		Format:    media.DefaultPCM16Format(),
	}

	got, err := stage.Process(context.Background(), frame)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got.SessionID != frame.SessionID {
		t.Fatalf("SessionID = %q, want %q", got.SessionID, frame.SessionID)
	}
	if got.Direction != frame.Direction {
		t.Fatalf("Direction = %q, want %q", got.Direction, frame.Direction)
	}
	if got.Format.Codec != frame.Format.Codec {
		t.Fatalf("Codec = %q, want %q", got.Format.Codec, frame.Format.Codec)
	}
	if string(got.Payload) != string(frame.Payload) {
		t.Fatalf("Payload = %#v, want %#v", got.Payload, frame.Payload)
	}
	if stage.Count() != 1 {
		t.Fatalf("Count = %d, want 1", stage.Count())
	}
	if err := stage.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestMockStageEmitsSilenceTimeout(t *testing.T) {
	stage := NewMockStage(nil)
	eventCh := make(chan media.StageEvent, 1)
	stage.SetEventEmitter(func(ctx context.Context, event media.StageEvent) {
		eventCh <- event
	})

	stage.EmitSilenceTimeout(context.Background(), media.Frame{
		SessionID: "client-a",
		Direction: media.DirectionUplink,
	})

	select {
	case event := <-eventCh:
		if event.Type != controller.EventSilenceTimeout {
			t.Fatalf("event type = %q", event.Type)
		}
		if event.Stage != stage.Name() {
			t.Fatalf("stage = %q", event.Stage)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}
