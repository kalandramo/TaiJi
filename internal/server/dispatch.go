package server

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 异步分发（issue #9 Wave 4）。
//
// 为什么必须异步：agent 跑一轮要数秒，远超飞书的事件响应窗口。
// 同步处理会让 HTTP 响应超时 → 平台重投 → 重复处理。
//
// 正确形态：**立即 200，后台处理**。但异步引入了两个新问题，
// 本文件分别处理：
//
//  1. 重投 → 用 channel.Deduper 幂等（同一条消息只处理一次）
//  2. 队列积压 → 有界队列 + 非阻塞投递。满了就报错让端点回 500，
//     平台会重试——这比无界队列吃光内存要好。

// ErrQueueFull 表示待处理队列已满。
//
// 端点收到它应回 5xx，让飞书重投。这不是「消息丢了」——
// 丢的是这次投递，消息仍在平台侧等待重试。
var ErrQueueFull = errors.New("server: dispatch queue is full")

// ErrDispatcherClosed 表示分发器已关闭。
var ErrDispatcherClosed = errors.New("server: dispatcher is closed")

// Handler 处理一条消息。Pipeline.Handle 满足它。
type Handler interface {
	Handle(ctx context.Context, msg *channel.IncomingMessage) error
}

// DispatcherConfig 是分发器装配参数。
type DispatcherConfig struct {
	// Handler 是实际的处理者（通常是 *Pipeline）。
	Handler Handler
	// Deduper 做消息幂等。nil 则不去重（不推荐——飞书会重投）。
	Deduper *channel.Deduper
	// QueueSize 是待处理队列容量。<=0 用 DefaultQueueSize。
	QueueSize int
	// Workers 是并发消费者数。<=0 用 DefaultWorkers。
	//
	// 注意：并发消费者不会破坏「同会话串行」——串行化由
	// Pipeline 内的 Serializer 保证（按 workspace 域互斥）。
	// 这里的并发只让不同域的消息并行处理。
	Workers int
	// Logf 是日志出口。
	Logf func(format string, args ...any)
}

// DefaultQueueSize 是默认队列容量。
//
// 取值考量：队列只是削峰，不是缓冲池。容量过大时，积压会让
// 用户等待过久却仍看不到「忙碌」信号；过小则频繁触发重投。
// 64 条足以吸收突发，又能让持续过载快速暴露。
const DefaultQueueSize = 64

// DefaultWorkers 是默认消费者数。
const DefaultWorkers = 4

// Dispatcher 把消息投递与处理解耦。
type Dispatcher struct {
	handler Handler
	deduper *channel.Deduper
	logf    func(format string, args ...any)

	queue chan *channel.IncomingMessage

	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
	stopped  chan struct{}
	stopOnce sync.Once
}

// NewDispatcher 装配并启动分发器。
//
// 启动即拉起 worker——调用方不需要单独 Start，避免「忘了启动」这类
// 静默失效（队列有消息却没人消费）。
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	if cfg.Handler == nil {
		return nil, errors.New("server: Dispatcher requires a Handler")
	}
	size := cfg.QueueSize
	if size <= 0 {
		size = DefaultQueueSize
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	d := &Dispatcher{
		handler: cfg.Handler,
		deduper: cfg.Deduper,
		logf:    logf,
		queue:   make(chan *channel.IncomingMessage, size),
		stopped: make(chan struct{}),
	}

	d.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go d.worker(i)
	}
	return d, nil
}

// Enqueue 投递一条消息，不阻塞。
//
// 去重在**投递时**做（而非消费时）：这样重复投递不会占用队列容量，
// 也不会让 worker 白跑一趟。
//
// 返回值语义：
//   - nil：已接收（或已识别为重复，无需处理——两者对调用方都是「不必重试」）
//   - ErrQueueFull：队列满，调用方应回 5xx 让平台重试
//   - ErrDispatcherClosed：已关闭，调用方应回 5xx
func (d *Dispatcher) Enqueue(msg *channel.IncomingMessage) error {
	if msg == nil {
		return errors.New("server: nil message")
	}

	// 幂等：重复投递直接吞掉（返回 nil），因为「不处理」正是期望结果。
	if d.deduper != nil && d.deduper.Seen(msg.MessageID) {
		d.logf("server: duplicate message ignored message_id=%s", msg.MessageID)
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrDispatcherClosed
	}

	select {
	case d.queue <- msg:
		return nil
	default:
		// 队列满：不阻塞。阻塞会让 HTTP 请求挂住，等于把同步问题
		// 换了个地方。让平台重试更诚实。
		return fmt.Errorf("%w (message_id=%s)", ErrQueueFull, msg.MessageID)
	}
}

// worker 消费队列。
func (d *Dispatcher) worker(id int) {
	defer d.wg.Done()
	for msg := range d.queue {
		// 每条消息独立 context：单条失败不影响其它，且进程退出时
		// 由 Stop 关闭队列让循环自然结束。
		ctx := context.Background()
		if err := d.handler.Handle(ctx, msg); err != nil {
			// 处理失败只记日志：HTTP 已回 200（消息已接收），
			// 无法再让平台重试。用户侧会看到管道发出的错误提示。
			d.logf("server: worker %d handle failed message_id=%s err=%v", id, msg.MessageID, err)
		}
	}
}

// Stop 关闭分发器并等待在途消息处理完。
//
// 先关队列（worker 循环自然退出），再等在途完成——顺序不可颠倒，
// 否则正在处理的消息会被丢弃。
func (d *Dispatcher) Stop() {
	d.stopOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		close(d.queue)
		d.mu.Unlock()

		d.wg.Wait()
		close(d.stopped)
	})
	<-d.stopped
}

// Pending 返回当前待处理的消息数（观测与测试用）。
func (d *Dispatcher) Pending() int {
	return len(d.queue)
}
