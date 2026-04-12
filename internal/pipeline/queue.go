package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"rtc-media-server/internal/log"
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

// ErrDropFrame 表示当前帧应被主动丢弃，不作为异常上报。
var ErrDropFrame = errors.New("drop pipeline frame")

// ErrorHandler 接收 pipeline 中需要上报给 Session 的处理错误。
type ErrorHandler func(ctx context.Context, frame media.Frame, err error)

// QueuePipeline 是一个有界队列驱动的串行媒体处理管线。
// 每个客户端的上行、下行都应拥有独立实例，避免跨客户端互相阻塞。
type QueuePipeline struct {
	name          string
	queue         chan media.Frame
	stages        []media.Stage
	sink          media.Sink
	errorStrategy ErrorStrategy
	errorHandler  ErrorHandler

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	processed atomic.Uint64
	errors    atomic.Uint64
}

// SetErrorHandler 设置 pipeline 错误上报回调。
func (p *QueuePipeline) SetErrorHandler(handler ErrorHandler) {
	p.errorHandler = handler
}

// NewQueuePipeline 创建一个有界队列 pipeline。
func NewQueuePipeline(name string, queueSize int, stages []media.Stage, sink media.Sink) *QueuePipeline {
	return NewQueuePipelineWithStrategy(name, queueSize, stages, sink, ErrorStrategyDrop)
}

// NewQueuePipelineWithStrategy 创建一个带错误策略的有界队列 pipeline。
func NewQueuePipelineWithStrategy(name string, queueSize int, stages []media.Stage, sink media.Sink, strategy ErrorStrategy) *QueuePipeline {
	if queueSize <= 0 {
		queueSize = 32
	}
	if strategy == "" {
		strategy = ErrorStrategyDrop
	}
	return &QueuePipeline{
		name:          name,
		queue:         make(chan media.Frame, queueSize),
		stages:        append([]media.Stage(nil), stages...),
		sink:          sink,
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

// run 从队列中串行取出媒体帧并执行完整 stage chain。
func (p *QueuePipeline) run() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case frame := <-p.queue:
			if err := p.process(p.ctx, frame); err != nil {
				p.errors.Add(1)
				log.Errorf(
					"client_id=%s pipeline process failed pipeline=%s direction=%s error=%v",
					frame.SessionID,
					p.name,
					frame.Direction,
					err,
				)
				continue
			}
			p.processed.Add(1)
		}
	}
}

// process 对单帧媒体依次执行 stage，并在成功后投递到 sink。
func (p *QueuePipeline) process(ctx context.Context, frame media.Frame) error {
	var err error
	for _, stage := range p.stages {
		frame, err = stage.Process(ctx, frame)
		if err != nil {
			if errors.Is(err, ErrDropFrame) {
				return nil
			}
			log.Errorf(
				"client_id=%s pipeline stage failed pipeline=%s stage=%s direction=%s codec=%s error=%v",
				frame.SessionID,
				p.name,
				stage.Name(),
				frame.Direction,
				frame.Format.Codec,
				err,
			)
			p.reportError(ctx, frame, fmt.Errorf("stage %s: %w", stage.Name(), err))
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
	if err := p.sink.Consume(ctx, frame); err != nil {
		p.reportError(ctx, frame, fmt.Errorf("sink: %w", err))
		return err
	}
	return nil
}

// reportError 调用外部错误处理器上报 pipeline 错误。
func (p *QueuePipeline) reportError(ctx context.Context, frame media.Frame, err error) {
	if p.errorHandler != nil {
		p.errorHandler(ctx, frame, err)
	}
}
