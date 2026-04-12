package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"rtc-media-server/internal/media"
)

func TestControllerDispatchesDownlinkReference(t *testing.T) {
	consumer := &referenceConsumer{}
	ctrl := New(Config{ReferenceQueueSize: 1}, Dependencies{
		SessionID: "client-a",
	})
	defer ctrl.Close(context.Background())
	ctrl.RegisterReferenceConsumer("aec", consumer)

	ctrl.OnDownlinkReference(context.Background(), media.Frame{
		SessionID: "client-a",
		Direction: media.DirectionDownlink,
		Payload:   []byte{0x01, 0x02},
		Format:    media.DefaultPCM16Format(),
	})

	deadline := time.After(2 * time.Second)
	for consumer.Count() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for reference dispatch")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestControllerSilenceTimeoutClosesSession(t *testing.T) {
	closed := make(chan string, 1)
	ctrl := New(Config{}, Dependencies{
		SessionID: "client-b",
		CloseSession: func(ctx context.Context, reason string) error {
			closed <- reason
			return nil
		},
	})
	defer ctrl.Close(context.Background())

	ctrl.Emit(context.Background(), media.StageEvent{
		SessionID: "client-b",
		Type:      EventSilenceTimeout,
		Direction: media.DirectionUplink,
		Stage:     "vad_mock",
	})

	select {
	case reason := <-closed:
		if reason != "silence timeout" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for close callback")
	}
}

func TestControllerDropsReferenceAfterClose(t *testing.T) {
	ctrl := New(Config{ReferenceQueueSize: 1}, Dependencies{
		SessionID: "client-c",
	})
	if err := ctrl.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done := make(chan struct{})
	go func() {
		ctrl.OnDownlinkReference(context.Background(), media.Frame{
			SessionID: "client-c",
			Direction: media.DirectionDownlink,
			Payload:   []byte{0x01},
			Format:    media.DefaultPCM16Format(),
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnDownlinkReference blocked after controller close")
	}
}

type referenceConsumer struct {
	count atomic.Uint64
}

func (c *referenceConsumer) AddReference(ctx context.Context, frame media.Frame) error {
	c.count.Add(1)
	return nil
}

func (c *referenceConsumer) Count() uint64 {
	return c.count.Load()
}
