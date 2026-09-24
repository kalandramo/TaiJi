package feishu

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 长连接测试（issue #9 Wave 5，AC-6）。
//
// AC-6 原文：「进程收到退出信号时，长连接（若启用）被真正关闭，不留存活 socket」。
//
// 关键不变量：**Stop 必须调用 client.Close()**，而不是只取消 ctx。
// 源码依据：larkws.Client.Start 末尾是裸 select{}（ws/client.go:206-232），
// 不观察 ctx；只有 Close（:177-180，设 autoReconnect=false 再 disconnect）
// 才真正断开。只取消 ctx 会让 socket 存活并自动重连。
//
// 用 fake wsClient 验证，因为真实 WebSocket 需要真实飞书端点。

// fakeWS 记录 Start/Close 的调用情况。
//
// **忠实模拟真实 SDK 的关键行为**：Start 永不返回。
// 源码依据：larkws.Client.Start 末尾是裸 select{}（ws/client.go:206-232），
// 它不观察 ctx，Close() 也不解除它的阻塞——Close 只是 disconnect socket。
//
// 早期版本的 fake 在 Close 时让 Start 返回，导致 Stop 里 `<-done` 的
// 等待看起来正常，而真实进程会永久挂死（issue #9 Wave 6 实测：
// Ctrl+C 后日志停在「正在关闭…」，socket 已断但进程不退出）。
// 教训：fake 在关键维度上偏离真实行为时，测试反而会掩盖缺陷。
type fakeWS struct {
	startCalled int32
	closeCalled int32
	// block 让 Start 阻塞，模拟 SDK 的 select{}。
	// **Close 不会关闭它**——Start 永不返回。
	block chan struct{}
}

func newFakeWS() *fakeWS {
	return &fakeWS{block: make(chan struct{})}
}

func (f *fakeWS) Start(ctx context.Context) error {
	atomic.StoreInt32(&f.startCalled, 1)
	// 模拟 SDK 的裸 select{}：永久阻塞，不观察 ctx，Close 也不解除。
	<-f.block
	return nil // 不可达：真实 SDK 下 select{} 永不返回
}

func (f *fakeWS) Close() {
	atomic.StoreInt32(&f.closeCalled, 1)
	// 真实 SDK 的 Close 只 disconnect socket，**不让 Start 返回**。
	// 此处刻意什么都不做——Start 会一直阻塞在 <-f.block。
}

// unblock 仅用于测试清理（释放 goroutine），不模拟 SDK 行为。
func (f *fakeWS) unblock() {
	select {
	case <-f.block:
	default:
		close(f.block)
	}
}

func (f *fakeWS) started() bool { return atomic.LoadInt32(&f.startCalled) == 1 }
func (f *fakeWS) closed() bool  { return atomic.LoadInt32(&f.closeCalled) == 1 }

// newTestLongConn 构造注入了 fake client 的长连接。
func newTestLongConn(t *testing.T, f *fakeWS, onMsg func(*channel.IncomingMessage) error) *LongConn {
	t.Helper()
	lc, err := NewLongConn(LongConnConfig{
		AppID:     "cli_fake",
		AppSecret: "sec_fake",
		OnMessage: onMsg,
		newClient: func(appID, appSecret string, handler *larkdispatcher.EventDispatcher) wsClient {
			return f
		},
	})
	if err != nil {
		t.Fatalf("NewLongConn: %v", err)
	}
	return lc
}

// ===== 装配校验 =====

func TestNewLongConn_RequiresCredentials(t *testing.T) {
	cases := []LongConnConfig{
		{AppSecret: "s"},
		{AppID: "a"},
		{},
	}
	for _, cfg := range cases {
		if _, err := NewLongConn(cfg); err == nil {
			t.Errorf("NewLongConn(%+v) must reject missing credentials", cfg)
		}
	}
}

// ===== AC-6 核心：Stop 真正关闭 =====

func TestLongConn_StopClosesConnection(t *testing.T) {
	// 这是 AC-6 的直接断言：Stop 必须调用 Close()。
	f := newFakeWS()
	lc := newTestLongConn(t, f, nil)

	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 等 Start 的 goroutine 真的进入阻塞
	if !waitForCond(t, time.Second, f.started) {
		t.Fatal("Start was not called")
	}

	lc.Stop()

	if !f.closed() {
		t.Error("Stop must call client.Close() — otherwise the socket stays alive (AC-6)")
	}
}

func TestLongConn_CancelAloneDoesNotStop(t *testing.T) {
	// **反向对照**：只取消 ctx 不够——这正是 AC-6 要防的「留存活 socket」。
	// 这条测试锁定「为什么必须 Close 而不是 cancel」这个设计判断。
	f := newFakeWS()
	lc := newTestLongConn(t, f, nil)

	ctx, cancel := context.WithCancel(context.Background())
	if err := lc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !waitForCond(t, time.Second, f.started) {
		t.Fatal("Start was not called")
	}

	cancel() // 只取消 ctx

	// 给足时间：若 fake 观察 ctx，它会返回；但按 SDK 语义它不该返回。
	time.Sleep(50 * time.Millisecond)

	if f.closed() {
		t.Error("cancel must not close the connection (SDK's Start ignores ctx)")
	}

	// 清理：必须用 Stop
	lc.Stop()
	if !f.closed() {
		t.Error("Stop must still work after cancel")
	}
}

func TestLongConn_StopReturnsEvenThoughStartNeverDoes(t *testing.T) {
	// **这是真实平台暴露的缺陷的回归护栏**（issue #9 Wave 6）。
	//
	// 源码事实：larkws.Client.Start 末尾是裸 select{}（ws/client.go:206-232），
	// 永不返回；Close() 只 disconnect socket，不解除 select{}。
	//
	// 因此 Stop **绝不能等待 Start 的 goroutine**——那会永久挂死。
	// 实测现象：Ctrl+C 后日志停在「收到中断信号，正在关闭…」，
	// socket 已断（日志有 disconnected + use of closed network connection），
	// 但进程不退出。
	//
	// 首版实现的 `<-l.done` 正是这个 bug；早期 fake 在 Close 时让 Start
	// 返回，掩盖了它。
	f := newFakeWS()
	lc := newTestLongConn(t, f, nil)

	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !waitForCond(t, time.Second, f.started) {
		t.Fatal("Start was not called")
	}

	// Stop 必须在有限时间内返回——否则进程无法退出。
	done := make(chan struct{})
	go func() {
		lc.Stop()
		close(done)
	}()

	select {
	case <-done:
		// 正确：Stop 不等待永不返回的 Start
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung waiting for Start's goroutine, which never returns " +
			"(SDK's Start ends in bare select{}). The process would never exit.")
	}

	if !f.closed() {
		t.Error("Stop must still call client.Close() to disconnect the socket")
	}

	// 清理：释放 fake 的 goroutine（真实 SDK 下它会一直阻塞到进程退出）
	f.unblock()
}

// ===== 生命周期幂等与非法状态 =====

func TestLongConn_StopIsIdempotent(t *testing.T) {
	f := newFakeWS()
	lc := newTestLongConn(t, f, nil)

	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lc.Stop()
	lc.Stop() // 不能 panic

	if !f.closed() {
		t.Error("Close must have been called")
	}
}

func TestLongConn_DoubleStartRejected(t *testing.T) {
	f := newFakeWS()
	lc := newTestLongConn(t, f, nil)

	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := lc.Start(context.Background()); err == nil {
		t.Error("second Start must be rejected")
	}
	lc.Stop()
}

func TestLongConn_StartAfterStopRejected(t *testing.T) {
	f := newFakeWS()
	lc := newTestLongConn(t, f, nil)
	lc.Stop() // 未 Start 就 Stop

	if err := lc.Start(context.Background()); err == nil {
		t.Error("Start after Stop must be rejected")
	}
}

// ===== 事件转换：长连接与 webhook 产出同一形状 =====

func TestIncomingFromLongConnEvent_FieldMapping(t *testing.T) {
	// 两条入站路径（长连接 vs webhook）必须产出语义一致的 IncomingMessage，
	// 否则下游（门禁/路由/执行）的行为会因入口不同而分叉。
	str := func(s string) *string { return &s }
	ev := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: str("ou_sender")},
			},
			Message: &larkim.EventMessage{
				MessageId: str("om_1"),
				ChatId:    str("oc_group"),
				ChatType:  str("group"),
				Content:   str(`{"text":"你好"}`),
			},
		},
	}

	msg := incomingFromLongConnEvent(ev)
	if msg == nil {
		t.Fatal("message must be produced")
	}
	if msg.UserID != "ou_sender" {
		t.Errorf("UserID = %q, want ou_sender", msg.UserID)
	}
	if msg.ChatID != "oc_group" {
		t.Errorf("ChatID = %q, want oc_group", msg.ChatID)
	}
	if msg.ChatType != channel.ChatGroup {
		t.Errorf("ChatType = %q, want group", msg.ChatType)
	}
	if msg.Content != "你好" {
		t.Errorf("Content = %q, want 你好 (content is a JSON string, must be unwrapped)", msg.Content)
	}
	if msg.MessageID != "om_1" {
		t.Errorf("MessageID = %q", msg.MessageID)
	}
	if msg.Platform != channel.PlatformFeishu {
		t.Errorf("Platform = %q", msg.Platform)
	}
	if msg.Meta == nil || msg.Meta.ChatType != "group" {
		t.Errorf("Meta.ChatType must keep the platform raw value, got %+v", msg.Meta)
	}
}

func TestIncomingFromLongConnEvent_ThreadSetsContextType(t *testing.T) {
	str := func(s string) *string { return &s }
	ev := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: str("ou_s")}},
			Message: &larkim.EventMessage{
				MessageId: str("om_1"),
				ChatId:    str("oc_g"),
				ChatType:  str("group"),
				Content:   str(`{"text":"hi"}`),
				ThreadId:  str("th_1"),
				RootId:    str("root_1"),
			},
		},
	}
	msg := incomingFromLongConnEvent(ev)
	if msg == nil || msg.Meta == nil {
		t.Fatal("message must be produced")
	}
	if msg.Meta.NativeContextType != "thread" {
		t.Errorf("NativeContextType = %q, want thread", msg.Meta.NativeContextType)
	}
	if msg.Meta.ThreadID != "th_1" || msg.Meta.RootID != "root_1" {
		t.Errorf("thread fields not mapped: %+v", msg.Meta)
	}
}

func TestIncomingFromLongConnEvent_DropsMessagesWithoutSender(t *testing.T) {
	// 无主体 ID 的消息无法参与权限判定——丢弃而非产出残缺消息
	// （与 parse.go 的 fail-closed 取向一致）。
	str := func(s string) *string { return &s }
	ev := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender:  &larkim.EventSender{}, // 无 SenderId
			Message: &larkim.EventMessage{MessageId: str("om_1"), ChatId: str("c")},
		},
	}
	if msg := incomingFromLongConnEvent(ev); msg != nil {
		t.Errorf("message without sender open_id must be dropped, got %+v", msg)
	}
}

func TestIncomingFromLongConnEvent_NilSafe(t *testing.T) {
	for _, ev := range []*larkim.P2MessageReceiveV1{nil, {}, {Event: &larkim.P2MessageReceiveV1Data{}}} {
		if msg := incomingFromLongConnEvent(ev); msg != nil {
			t.Errorf("nil/incomplete event must yield nil, got %+v", msg)
		}
	}
}

func TestIncomingFromLongConnEvent_P2PUsesDirect(t *testing.T) {
	str := func(s string) *string { return &s }
	ev := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: str("ou_s")}},
			Message: &larkim.EventMessage{
				MessageId: str("om_1"),
				ChatType:  str("p2p"),
				Content:   str(`{"text":"hi"}`),
			},
		},
	}
	msg := incomingFromLongConnEvent(ev)
	if msg == nil {
		t.Fatal("message must be produced")
	}
	if msg.ChatType != channel.ChatDirect {
		t.Errorf("ChatType = %q, want direct (p2p must normalize to direct)", msg.ChatType)
	}
}

// ===== Start 错误传播 =====

func TestLongConn_StartErrorRecorded(t *testing.T) {
	// Start 的错误必须可查——SDK 在**连接失败**时会 `return err`
	// （ws/client.go：connect 失败且不可重连时返回，不进入 select{}）。
	//
	// 用 failFastWS 模拟这条路径（连接失败 → Start 立即返回错误），
	// 而非 fakeWS 的「连接成功 → 永久阻塞」。
	f := &failFastWS{err: errors.New("connect refused")}
	lc, err := NewLongConn(LongConnConfig{
		AppID:     "a",
		AppSecret: "b",
		newClient: func(appID, appSecret string, handler *larkdispatcher.EventDispatcher) wsClient {
			return f
		},
	})
	if err != nil {
		t.Fatalf("NewLongConn: %v", err)
	}

	if err := lc.Start(context.Background()); err != nil {
		t.Fatalf("Start (launch) must not fail synchronously: %v", err)
	}
	// 等 Start 的 goroutine 记录错误（它是异步的）
	if !waitForCond(t, 2*time.Second, func() bool { return lc.StartErr() != nil }) {
		t.Fatal("Start's error must be recorded for inspection")
	}
	lc.Stop()
}

// failFastWS 模拟「连接失败 → Start 立即返回错误」的 SDK 路径。
type failFastWS struct{ err error }

func (f *failFastWS) Start(ctx context.Context) error { return f.err }
func (f *failFastWS) Close()                          {}

// waitForCond 轮询等待条件成立。
func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) bool {
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

// 编译期断言：真实 SDK 客户端满足 wsClient 契约。
var _ wsClient = larkWSAdapter{}

// 保证 sync 被使用（mu 在 LongConn 中）。
var _ = sync.Mutex{}
