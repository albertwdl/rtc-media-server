package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"rtc-media-server/internal/media"
)

const (
	testTimeout   = time.Second
	testQueueSize = 4
)

// TestQueuePipelineStageNodesRunIndependently 验证不同 stageNode 由独立 goroutine 驱动。
func TestQueuePipelineStageNodesRunIndependently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstSeen := make(chan uint64, 2)
	secondStageEntered := make(chan struct{})
	releaseSecondStage := make(chan struct{})

	first := media.NewStageFunc("first", func(ctx context.Context, frame media.Frame) (media.Frame, error) {
		firstSeen <- frame.Seq
		return frame, nil
	})

	var blockOnce bool
	second := media.NewStageFunc("second", func(ctx context.Context, frame media.Frame) (media.Frame, error) {
		if !blockOnce {
			blockOnce = true
			close(secondStageEntered)
			<-releaseSecondStage
		}
		return frame, nil
	})

	received := make(chan media.Frame, 2)
	p := NewQueuePipeline("test", testQueueSize, []media.Stage{first, second}, media.OutputFunc(func(ctx context.Context, frame media.Frame) error {
		received <- frame
		return nil
	}))
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	if err := p.Push(ctx, media.Frame{SessionID: "client-a", Seq: 1}); err != nil {
		t.Fatalf("Push first frame: %v", err)
	}
	waitClosed(t, secondStageEntered, "second stage did not receive first frame")

	if got := waitSeq(t, firstSeen, "first stage did not process first frame"); got != 1 {
		t.Fatalf("first seen seq = %d, want 1", got)
	}
	if err := p.Push(ctx, media.Frame{SessionID: "client-a", Seq: 2}); err != nil {
		t.Fatalf("Push second frame: %v", err)
	}
	if got := waitSeq(t, firstSeen, "first stage did not continue while second stage was blocked"); got != 2 {
		t.Fatalf("first seen seq = %d, want 2", got)
	}

	close(releaseSecondStage)
	if got := waitFrame(t, received, "output did not receive first frame"); got.Seq != 1 {
		t.Fatalf("received seq = %d, want 1", got.Seq)
	}
	if got := waitFrame(t, received, "output did not receive second frame"); got.Seq != 2 {
		t.Fatalf("received seq = %d, want 2", got.Seq)
	}
}

// TestQueuePipelineWithoutStages 验证没有 stage 时输入帧会直接进入 outputNode。
func TestQueuePipelineWithoutStages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan media.Frame, 1)
	p := NewQueuePipeline("test", testQueueSize, nil, media.OutputFunc(func(ctx context.Context, frame media.Frame) error {
		received <- frame
		return nil
	}))
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	if err := p.Push(ctx, media.Frame{SessionID: "client-a", Seq: 7}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := waitFrame(t, received, "output did not receive frame"); got.Seq != 7 {
		t.Fatalf("received seq = %d, want 7", got.Seq)
	}
}

// TestQueuePipelineDropFrame 验证 ErrDropFrame 会终止当前帧且不触发 output。
func TestQueuePipelineDropFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stage := media.NewStageFunc("drop", func(ctx context.Context, frame media.Frame) (media.Frame, error) {
		return frame, ErrDropFrame
	})
	received := make(chan media.Frame, 1)
	p := NewQueuePipeline("test", testQueueSize, []media.Stage{stage}, media.OutputFunc(func(ctx context.Context, frame media.Frame) error {
		received <- frame
		return nil
	}))
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	if err := p.Push(ctx, media.Frame{SessionID: "client-a"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	select {
	case frame := <-received:
		t.Fatalf("output received dropped frame: %+v", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestQueuePipelineContinueStrategy 验证 continue 策略会把原始帧继续传递给后续节点。
func TestQueuePipelineContinueStrategy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stageErr := errors.New("stage failed")
	stage := media.NewStageFunc("fail", func(ctx context.Context, frame media.Frame) (media.Frame, error) {
		frame.Seq = 99
		return frame, stageErr
	})
	received := make(chan media.Frame, 1)
	reported := make(chan error, 1)
	p := NewQueuePipelineWithStrategy("test", testQueueSize, []media.Stage{stage}, media.OutputFunc(func(ctx context.Context, frame media.Frame) error {
		received <- frame
		return nil
	}), ErrorStrategyContinue)
	p.SetErrorHandler(func(ctx context.Context, frame media.Frame, err error) {
		reported <- err
	})
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	if err := p.Push(ctx, media.Frame{SessionID: "client-a", Seq: 3}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := waitFrame(t, received, "output did not receive continued frame"); got.Seq != 3 {
		t.Fatalf("continued seq = %d, want original seq 3", got.Seq)
	}
	if err := waitErr(t, reported, "error handler was not called"); !errors.Is(err, stageErr) {
		t.Fatalf("reported err = %v, want %v", err, stageErr)
	}
}

// TestQueuePipelineCloseRejectsPush 验证关闭后 pipeline 不再接收新帧。
func TestQueuePipelineCloseRejectsPush(t *testing.T) {
	p := NewQueuePipeline("test", testQueueSize, nil, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Push(context.Background(), media.Frame{SessionID: "client-a"}); !errors.Is(err, errPipelineClosed) {
		t.Fatalf("Push after Close err = %v, want %v", err, errPipelineClosed)
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testTimeout):
		t.Fatal(msg)
	}
}

func waitSeq(t *testing.T, ch <-chan uint64, msg string) uint64 {
	t.Helper()
	select {
	case seq := <-ch:
		return seq
	case <-time.After(testTimeout):
		t.Fatal(msg)
		return 0
	}
}

func waitFrame(t *testing.T, ch <-chan media.Frame, msg string) media.Frame {
	t.Helper()
	select {
	case frame := <-ch:
		return frame
	case <-time.After(testTimeout):
		t.Fatal(msg)
		return media.Frame{}
	}
}

func waitErr(t *testing.T, ch <-chan error, msg string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(testTimeout):
		t.Fatal(msg)
		return nil
	}
}
