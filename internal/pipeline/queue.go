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

const (
	// DefaultQueueSize 是每条 pipeline 默认使用的有界队列长度。
	DefaultQueueSize = 32
)

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

// QueuePipeline 是一个由 stageNode 串联的有界队列媒体处理管线。
// 每个 stage 拥有独立 goroutine，避免慢 stage 阻塞整条管线的前序处理。
type QueuePipeline struct {
	name          string
	input         media.Input
	nodes         []*stageNode
	outputNode    *outputNode
	stages        []media.Stage
	errorStrategy ErrorStrategy

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	errorMu      sync.RWMutex
	errorHandler ErrorHandler

	processed atomic.Uint64
	errors    atomic.Uint64
}

// SetErrorHandler 设置 pipeline 错误上报回调。
func (p *QueuePipeline) SetErrorHandler(handler ErrorHandler) {
	p.errorMu.Lock()
	defer p.errorMu.Unlock()
	p.errorHandler = handler
}

// NewQueuePipeline 创建一个有界队列 pipeline。
func NewQueuePipeline(name string, queueSize int, stages []media.Stage, output media.Output) *QueuePipeline {
	return NewQueuePipelineWithStrategy(name, queueSize, stages, output, ErrorStrategyDrop)
}

// NewQueuePipelineWithStrategy 创建一个带错误策略的有界队列 pipeline。
func NewQueuePipelineWithStrategy(name string, queueSize int, stages []media.Stage, output media.Output, strategy ErrorStrategy) *QueuePipeline {
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	if strategy == "" {
		strategy = ErrorStrategyDrop
	}
	p := &QueuePipeline{
		name:          name,
		stages:        append([]media.Stage(nil), stages...),
		errorStrategy: strategy,
		done:          make(chan struct{}),
	}
	p.buildNodes(queueSize, output)
	return p
}

// Start 启动 pipeline 中所有 stageNode 和 outputNode 的处理 goroutine。
func (p *QueuePipeline) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	for _, node := range p.nodes {
		node.start(p.ctx)
	}
	p.outputNode.start(p.ctx)
	return nil
}

// Push 向 pipeline 输入节点投递媒体帧。入口队列满时会阻塞调用方，形成当前 Session 内部背压。
func (p *QueuePipeline) Push(ctx context.Context, frame media.Frame) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return p.input.Push(ctx, frame)
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
	queued := p.outputNode.queueLen()
	for _, node := range p.nodes {
		queued += node.queueLen()
	}
	return media.Stats{
		Queued:    queued,
		Processed: p.processed.Load(),
		Errors:    p.errors.Load(),
	}
}

// buildNodes 按 stage 顺序构建内部节点链路。
func (p *QueuePipeline) buildNodes(queueSize int, output media.Output) {
	p.outputNode = &outputNode{
		pipeline:  p.name,
		input:     make(chan media.Frame, queueSize),
		output:    output,
		done:      p.done,
		onError:   p.reportError,
		close:     p.Close,
		strategy:  p.errorStrategy,
		processed: &p.processed,
		errors:    &p.errors,
	}

	next := media.Input(p.outputNode)
	p.nodes = make([]*stageNode, len(p.stages))
	for i := len(p.stages) - 1; i >= 0; i-- {
		node := &stageNode{
			pipeline:  p.name,
			stage:     p.stages[i],
			input:     make(chan media.Frame, queueSize),
			next:      next,
			done:      p.done,
			onError:   p.reportError,
			close:     p.Close,
			strategy:  p.errorStrategy,
			processed: &p.processed,
			errors:    &p.errors,
		}
		p.nodes[i] = node
		next = node
	}
	p.input = next
}

// reportError 调用外部错误处理器上报 pipeline 错误。
func (p *QueuePipeline) reportError(ctx context.Context, frame media.Frame, err error) {
	p.errorMu.RLock()
	handler := p.errorHandler
	p.errorMu.RUnlock()
	if handler != nil {
		handler(ctx, frame, err)
	}
}

// stageNode 封装单个 stage 的输入队列、goroutine 和错误处理逻辑。
type stageNode struct {
	pipeline string
	stage    media.Stage
	input    chan media.Frame
	next     media.Input
	done     <-chan struct{}

	strategy ErrorStrategy
	onError  ErrorHandler
	close    func(context.Context) error

	processed *atomic.Uint64
	errors    *atomic.Uint64
}

// Push 向当前 stageNode 的输入队列投递媒体帧。
func (n *stageNode) Push(ctx context.Context, frame media.Frame) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-n.done:
		return errPipelineClosed
	case <-ctx.Done():
		return ctx.Err()
	case n.input <- frame:
		return nil
	}
}

// start 启动当前 stageNode 的处理 goroutine。
func (n *stageNode) start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-n.done:
				return
			case frame := <-n.input:
				n.process(ctx, frame)
			}
		}
	}()
}

// queueLen 返回当前节点输入队列长度。
func (n *stageNode) queueLen() int {
	return len(n.input)
}

// process 执行当前 stage，并把成功处理后的帧推给下一个节点。
func (n *stageNode) process(ctx context.Context, frame media.Frame) {
	original := frame
	nextFrame, err := n.stage.Process(ctx, frame)
	if err != nil {
		n.handleError(ctx, original, err)
		return
	}
	if err := n.next.Push(ctx, nextFrame); err != nil {
		n.errors.Add(1)
		n.report(ctx, nextFrame, fmt.Errorf("stage %s forward: %w", n.stage.Name(), err))
	}
}

// handleError 根据 pipeline 错误策略处理当前 stage 的异常。
func (n *stageNode) handleError(ctx context.Context, frame media.Frame, err error) {
	if errors.Is(err, ErrDropFrame) {
		n.processed.Add(1)
		return
	}
	n.errors.Add(1)
	log.Errorf(
		"client_id=%s pipeline stage failed pipeline=%s stage=%s direction=%s codec=%s error=%v",
		frame.SessionID,
		n.pipeline,
		n.stage.Name(),
		frame.Direction,
		frame.Format.Codec,
		err,
	)
	n.report(ctx, frame, fmt.Errorf("stage %s: %w", n.stage.Name(), err))
	if errors.Is(err, ErrFatal) || n.strategy == ErrorStrategyCloseSession {
		_ = n.close(ctx)
		return
	}
	if n.strategy == ErrorStrategyContinue {
		if forwardErr := n.next.Push(ctx, frame); forwardErr != nil {
			n.errors.Add(1)
			n.report(ctx, frame, fmt.Errorf("stage %s forward: %w", n.stage.Name(), forwardErr))
		}
		return
	}
	n.processed.Add(1)
}

// report 上报当前节点内产生的 pipeline 错误。
func (n *stageNode) report(ctx context.Context, frame media.Frame, err error) {
	if n.onError != nil {
		n.onError(ctx, frame, err)
	}
}

// outputNode 封装 pipeline 最终输出端的有界队列和发送 goroutine。
type outputNode struct {
	pipeline string
	input    chan media.Frame
	output   media.Output
	done     <-chan struct{}

	strategy ErrorStrategy
	onError  ErrorHandler
	close    func(context.Context) error

	processed *atomic.Uint64
	errors    *atomic.Uint64
}

// Push 向最终输出节点投递已经完成 stage chain 的媒体帧。
func (n *outputNode) Push(ctx context.Context, frame media.Frame) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-n.done:
		return errPipelineClosed
	case <-ctx.Done():
		return ctx.Err()
	case n.input <- frame:
		return nil
	}
}

// start 启动最终输出节点的发送 goroutine。
func (n *outputNode) start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-n.done:
				return
			case frame := <-n.input:
				n.send(ctx, frame)
			}
		}
	}()
}

// queueLen 返回最终输出节点输入队列长度。
func (n *outputNode) queueLen() int {
	return len(n.input)
}

// send 把处理完成的媒体帧发送到 pipeline 输出端。
func (n *outputNode) send(ctx context.Context, frame media.Frame) {
	if n.output == nil {
		n.processed.Add(1)
		return
	}
	if err := n.output.SendData(ctx, frame); err != nil {
		n.errors.Add(1)
		log.Errorf(
			"client_id=%s pipeline output failed pipeline=%s direction=%s codec=%s error=%v",
			frame.SessionID,
			n.pipeline,
			frame.Direction,
			frame.Format.Codec,
			err,
		)
		n.report(ctx, frame, fmt.Errorf("output: %w", err))
		if n.strategy == ErrorStrategyCloseSession {
			_ = n.close(ctx)
		}
		return
	}
	n.processed.Add(1)
}

// report 上报最终输出节点产生的 pipeline 错误。
func (n *outputNode) report(ctx context.Context, frame media.Frame, err error) {
	if n.onError != nil {
		n.onError(ctx, frame, err)
	}
}
