package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 异步分发测试（issue #9 Wave 4）。
//
// 核心不变量：
//  1. 重复投递只处理一次（抗飞书重投）
//  2. 队列满时不阻塞、返回可识别错误（让端点回 5xx 触发重试）
//  3. Stop 等待在途消息处理完（不丢已接收的消息）

// countingHandler 记录处理过的消息。
type countingHandler struct {
	mu    sync.Mutex
	seen  []string
	delay time.Duration
	err   error
}

func (h *countingHandler) Handle(ctx context.Context, msg *channel.IncomingMessage) error {
	if h.delay > 0 {
		time.Sleep(h.delay)
	}
	h.mu.Lock()
	h.seen = append(h.seen, msg.MessageID)
	err := h.err
	h.mu.Unlock()
	return err
}

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.seen)
}

func (h *countingHandler) ids() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func msgWith(id string) *channel.IncomingMessage {
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    "ou_u",
		ChatType:  channel.ChatDirect,
		MessageID: id,
		Content:   "hi",
	}
}

// waitFor 轮询等待条件成立，避免固定 sleep 造成的慢测试与 flake。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// ===== 基本投递 =====

func TestDispatcher_ProcessesEnqueuedMessage(t *testing.T) {
	h := &countingHandler{}
	d, err := NewDispatcher(DispatcherConfig{Handler: h})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Stop()

	if err := d.Enqueue(msgWith("om_1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return h.count() == 1 }) {
		t.Fatalf("message not processed, seen = %v", h.ids())
	}
}

// ===== 去重（抗飞书重投） =====

func TestDispatcher_DeduplicatesRepeatedDelivery(t *testing.T) {
	// 飞书是至少一次投递：同一 message_id 可能到达多次。
	// 只有第一次应被处理。
	h := &countingHandler{}
	d, err := NewDispatcher(DispatcherConfig{
		Handler: h,
		Deduper: channel.NewDeduper(time.Minute),
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Stop()

	for i := 0; i < 5; i++ {
		if err := d.Enqueue(msgWith("om_dup")); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// 等到至少处理过一次，再给足时间去暴露重复
	if !waitFor(t, 2*time.Second, func() bool { return h.count() >= 1 }) {
		t.Fatal("message not processed at all")
	}
	time.Sleep(100 * time.Millisecond)

	if n := h.count(); n != 1 {
		t.Errorf("handler invocations = %d, want 1 (duplicates must be collapsed)", n)
	}
}

func TestDispatcher_DuplicateDoesNotConsumeQueueCapacity(t *testing.T) {
	// 去重在投递时做——重复投递不该占队列容量，否则重投风暴会把
	// 真正的新消息挤掉。
	h := &countingHandler{delay: 200 * time.Millisecond} // 拖住 worker，让队列保持有内容
	d, err := NewDispatcher(DispatcherConfig{
		Handler:   h,
		Deduper:   channel.NewDeduper(time.Minute),
		QueueSize: 2,
		Workers:   1,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Stop()

	// 填满队列
	_ = d.Enqueue(msgWith("a"))
	_ = d.Enqueue(msgWith("b"))

	// 重复投递不应因队列满而报错（它在去重阶段就被吞掉）
	if err := d.Enqueue(msgWith("a")); err != nil {
		t.Errorf("duplicate enqueue must not fail even when queue is full, got %v", err)
	}
}

// ===== 队列满（背压） =====

func TestDispatcher_QueueFullReturnsErrorNotBlock(t *testing.T) {
	// 队列满时必须快速失败，不能阻塞——阻塞会让 HTTP 请求挂住，
	// 等于把同步问题换个地方。端点据 ErrQueueFull 回 5xx 让平台重试。
	block := make(chan struct{})
	h := &blockingHandler{release: block}
	d, err := NewDispatcher(DispatcherConfig{
		Handler:   h,
		QueueSize: 1,
		Workers:   1,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	// 第一条被 worker 取走并阻塞，第二条占住队列
	_ = d.Enqueue(msgWith("a"))
	if !waitFor(t, time.Second, func() bool { return h.started() }) {
		t.Fatal("worker did not start")
	}
	_ = d.Enqueue(msgWith("b"))

	// 第三条应因队列满而报错，且**立即**返回
	done := make(chan error, 1)
	go func() { done <- d.Enqueue(msgWith("c")) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueFull) {
			t.Errorf("err = %v, want ErrQueueFull", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked on a full queue (must fail fast)")
	}

	close(block)
	d.Stop()
}

// blockingHandler 阻塞在 release 上，用于制造队列满。
type blockingHandler struct {
	release chan struct{}
	once    sync.Once
	n       int32
}

func (h *blockingHandler) Handle(ctx context.Context, msg *channel.IncomingMessage) error {
	atomic.AddInt32(&h.n, 1)
	<-h.release
	return nil
}

func (h *blockingHandler) started() bool { return atomic.LoadInt32(&h.n) > 0 }

// ===== Stop 语义 =====

func TestDispatcher_StopWaitsForInflight(t *testing.T) {
	// Stop 必须等在途消息处理完——否则已接收（HTTP 已回 200）的消息
	// 会被静默丢弃，用户永远等不到回复。
	h := &countingHandler{delay: 80 * time.Millisecond}
	d, err := NewDispatcher(DispatcherConfig{Handler: h, Workers: 1})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if err := d.Enqueue(msgWith("om_1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !waitFor(t, time.Second, func() bool { return d.Pending() == 0 }) {
		t.Fatal("worker did not pick up the message")
	}

	d.Stop() // 应等待处理完成

	if n := h.count(); n != 1 {
		t.Errorf("handler invocations = %d, want 1 (Stop must wait for inflight)", n)
	}
}

func TestDispatcher_EnqueueAfterStopFails(t *testing.T) {
	h := &countingHandler{}
	d, err := NewDispatcher(DispatcherConfig{Handler: h})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	d.Stop()

	if err := d.Enqueue(msgWith("om_1")); !errors.Is(err, ErrDispatcherClosed) {
		t.Errorf("err = %v, want ErrDispatcherClosed", err)
	}
}

func TestDispatcher_StopIsIdempotent(t *testing.T) {
	// 关闭路径可能被多处触发（signal + 错误分支），重复调用不能 panic。
	d, err := NewDispatcher(DispatcherConfig{Handler: &countingHandler{}})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	d.Stop()
	d.Stop()
}

// ===== 并发与错误隔离 =====

func TestDispatcher_ConcurrentEnqueueAllProcessed(t *testing.T) {
	h := &countingHandler{}
	d, err := NewDispatcher(DispatcherConfig{
		Handler:   h,
		Deduper:   channel.NewDeduper(time.Minute),
		QueueSize: 256,
		// 单 worker：多 worker 破坏同会话顺序（见 DefaultWorkers 说明）。
		// 本条测的是「并发入队不丢消息」，与 worker 数无关。
		Workers: 1,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Stop()

	const n = 50
	var wg sync.WaitGroup
	var failed int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := d.Enqueue(msgWith(idOf(i))); err != nil {
				atomic.AddInt32(&failed, 1)
			}
		}(i)
	}
	wg.Wait()

	if !waitFor(t, 3*time.Second, func() bool { return h.count() == n-int(atomic.LoadInt32(&failed)) }) {
		t.Errorf("processed = %d, enqueued ok = %d", h.count(), n-int(atomic.LoadInt32(&failed)))
	}
}

func idOf(i int) string {
	return "om_" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}

func TestDispatcher_HandlerErrorDoesNotStopWorkers(t *testing.T) {
	// 单条失败不能拖垮整个 worker——后续消息仍须被处理。
	h := &countingHandler{err: errors.New("boom")}
	d, err := NewDispatcher(DispatcherConfig{Handler: h, Workers: 1})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Stop()

	for _, id := range []string{"a", "b", "c"} {
		if err := d.Enqueue(msgWith(id)); err != nil {
			t.Fatalf("Enqueue %s: %v", id, err)
		}
	}
	if !waitFor(t, 2*time.Second, func() bool { return h.count() == 3 }) {
		t.Errorf("processed = %d, want 3 (one failure must not stop the worker)", h.count())
	}
}

// ===== 装配校验 =====

func TestNewDispatcher_RequiresHandler(t *testing.T) {
	if _, err := NewDispatcher(DispatcherConfig{}); err == nil {
		t.Error("NewDispatcher must reject a nil Handler")
	}
}

func TestNewDispatcher_RejectsMultipleWorkers(t *testing.T) {
	// 多 worker 破坏同会话消息顺序：N 个 worker 并发取消息，谁先抢到
	// 串行化域不确定。串行化器是 FIFO 的，但只保证「已到达的调用者」
	// 之间的顺序——两个 worker 谁先调用它取决于调度。
	//
	// 必须**显式拒绝**而非静默降到 1：静默降级会让调用方以为并发已生效。
	//
	// 实测依据：workers=4 时 TestE2E_SequentialMessagesInSameSession
	// 10 次跑出 4 次失败（execution order = [第二句 第一句]）。
	if _, err := NewDispatcher(DispatcherConfig{
		Handler: &countingHandler{},
		Workers: 4,
	}); err == nil {
		t.Error("NewDispatcher must reject Workers > 1 (breaks per-session ordering)")
	}
	// 边界：1 与 0（默认）都合法
	for _, w := range []int{0, 1} {
		d, err := NewDispatcher(DispatcherConfig{Handler: &countingHandler{}, Workers: w})
		if err != nil {
			t.Errorf("Workers=%d must be accepted, got %v", w, err)
		}
		d.Stop()
	}
}

func TestDispatcher_NilMessageRejected(t *testing.T) {
	d, err := NewDispatcher(DispatcherConfig{Handler: &countingHandler{}})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Stop()
	if err := d.Enqueue(nil); err == nil {
		t.Error("nil message must be rejected")
	}
}
