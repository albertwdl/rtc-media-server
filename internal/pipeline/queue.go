package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"rtc-media-server/internal/media"
)

var errPipelineClosed = errors.New("pipeline closed")

// ErrorStrategy 定义 stage 处理失败后的 pipeline 行为。
type ErrorStrategy string

const (
	ErrorStrategyDrop         ErrorStrategy = "drop"
	ErrorStrategyContinue     ErrorStrategy = "continue"
	ErrorStrategyCloseSession ErrorStrategy = "close_session"
)

// ErrFatal 表示不可恢复的媒体处理错误，pipeline 会优先关闭当前实例。
var ErrFatal = errors.New("fatal pipeline error")

// QueuePipeline 是一个有界队列驱动的串行媒体处理管线。
// 每个客户端的上行、下行都应拥有独立实例，避免跨客户端互相阻塞。
type QueuePipeline struct {
	name          string
	queue         chan media.Frame
	stages        []media.Stage
	sink          media.Sink
	logger        *slog.Logger
	errorStrategy ErrorStrategy

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	processed atomic.Uint64
	errors    atomic.Uint64
}

// NewQueuePipeline 创建一个有界队列 pipeline。
func NewQueuePipeline(name string, queueSize int, stages []media.Stage, sink media.Sink, logger *slog.Logger) *QueuePipeline {
	return NewQueuePipelineWithStrategy(name, queueSize, stages, sink, logger, ErrorStrategyDrop)
}

// NewQueuePipelineWithStrategy 创建一个带错误策略的有界队列 pipeline。
func NewQueuePipelineWithStrategy(name string, queueSize int, stages []media.Stage, sink media.Sink, logger *slog.Logger, strategy ErrorStrategy) *QueuePipeline {
	if queueSize <= 0 {
		queueSize = 32
	}
	if logger == nil {
		logger = slog.Default()
	}
	if strategy == "" {
		strategy = ErrorStrategyDrop
	}
	return &QueuePipeline{
		name:          name,
		queue:         make(chan media.Frame, queueSize),
		stages:        append([]media.Stage(nil), stages...),
		sink:          sink,
		logger:        logger,
		errorStrategy: strategy,
		done:          make(chan struct{}),
	}
}

// Start 启动 pipeline 处理 goroutine。
func (p *QueuePipeline) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	go p.run()
	return nil
}

// Push 投递媒体帧。队列满时会阻塞调用方，形成当前 Session 内部背压。
func (p *QueuePipeline) Push(ctx context.Context, frame media.Frame) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-p.done:
		return errPipelineClosed
	case <-ctx.Done():
		return ctx.Err()
	case p.queue <- frame:
		return nil
	}
}

// Close 关闭 pipeline，不再接收新帧。
func (p *QueuePipeline) Close(ctx context.Context) error {
	var firstErr error
	p.once.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
		close(p.done)
		for _, stage := range p.stages {
			if err := stage.Close(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// Stats 返回 pipeline 基础统计。
func (p *QueuePipeline) Stats() media.Stats {
	return media.Stats{
		Queued:    len(p.queue),
		Processed: p.processed.Load(),
		Errors:    p.errors.Load(),
	}
}

func (p *QueuePipeline) run() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case frame := <-p.queue:
			if err := p.process(p.ctx, frame); err != nil {
				p.errors.Add(1)
				p.logger.Error(
					fmt.Sprintf("client_id=%s pipeline process failed", frame.SessionID),
					slog.String("client_id", frame.SessionID),
					slog.String("pipeline", p.name),
					slog.String("direction", string(frame.Direction)),
					slog.Any("error", err),
				)
				continue
			}
			p.processed.Add(1)
		}
	}
}

func (p *QueuePipeline) process(ctx context.Context, frame media.Frame) error {
	var err error
	for _, stage := range p.stages {
		frame, err = stage.Process(ctx, frame)
		if err != nil {
			p.logger.Error(
				fmt.Sprintf("client_id=%s pipeline stage failed", frame.SessionID),
				slog.String("client_id", frame.SessionID),
				slog.String("pipeline", p.name),
				slog.String("stage", stage.Name()),
				slog.String("direction", string(frame.Direction)),
				slog.String("codec", frame.Format.Codec),
				slog.Any("error", err),
			)
			if errors.Is(err, ErrFatal) || p.errorStrategy == ErrorStrategyCloseSession {
				_ = p.Close(ctx)
				return err
			}
			if p.errorStrategy == ErrorStrategyContinue {
				continue
			}
			return nil
		}
	}
	if p.sink == nil {
		return nil
	}
	return p.sink.Consume(ctx, frame)
}
