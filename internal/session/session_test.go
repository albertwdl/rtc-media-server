package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"rtc-media-server/internal/media"
)

func TestAttachRequiresServiceConnector(t *testing.T) {
	manager := NewManager(Config{}, Dependencies{})
	client := &testClientConnector{id: "client-a", done: make(chan struct{})}

	_, _, err := manager.Attach(context.Background(), client)
	if err == nil {
		t.Fatal("Attach returned nil error")
	}
	if !strings.Contains(err.Error(), "NewServiceConnector") {
		t.Fatalf("error = %v", err)
	}
}

type testClientConnector struct {
	id   string
	done chan struct{}
}

func (c *testClientConnector) ID() string { return c.id }

func (c *testClientConnector) Protocol() string { return "test_client" }

func (c *testClientConnector) Start(ctx context.Context, sink media.Sink) error { return nil }

func (c *testClientConnector) Consume(ctx context.Context, frame media.Frame) error { return nil }

func (c *testClientConnector) MeasureRTT(ctx context.Context) (time.Duration, error) {
	return time.Millisecond, nil
}

func (c *testClientConnector) Close(ctx context.Context, reason string) error { return nil }

func (c *testClientConnector) Done() <-chan struct{} { return c.done }
