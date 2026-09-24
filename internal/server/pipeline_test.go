package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/channel"
)

// 管道装配测试（issue #9 Wave 3）。
//
// 管道把各层串起来：门禁 → 路由 → 串行化 → 执行 → 出站。
// 本文件用 fake Sender + fake Executor 验证**接线**正确，
// 尤其是 AC-4 的「门禁拒绝时不产生任何副作用」——这需要双向断言：
// 既没有出站调用，也没有 run 启动。只断言其一不足以证明。

// fakeSender 记录所有出站调用。
type fakeSender struct {
	mu    sync.Mutex
	calls []sendCall
	err   error
	// nextID 模拟飞书返回的消息 ID 序列。
	nextID int
}

type sendCall struct {
	To        string
	Text      string
	IDType    channel.ReceiveIDType
	MessageID string // 非空 = 更新
}

func (f *fakeSender) SendMessage(ctx context.Context, to, text string, opts channel.SendOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, sendCall{
		To:        to,
		Text:      text,
		IDType:    opts.ReceiveIDType,
		MessageID: opts.MessageID,
	})
	if opts.MessageID != "" {
		return opts.MessageID, nil
	}
	f.nextID++
	return "om_fake", nil
}

func (f *fakeSender) snapshot() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendCall(nil), f.calls...)
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeExecutor 记录所有执行调用，并可注入延迟/错误。
type fakeExecutor struct {
	mu   sync.Mutex
	runs []string
	// sessions 记录每次执行收到的 sessionID（AC-3 的会话隔离断言）。
	sessions []string
	err      error
	delay    time.Duration
	// events 记录 enter/exit，用于断言串行化。
	events []string
}

func (f *fakeExecutor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	f.mu.Lock()
	f.events = append(f.events, "enter:"+input)
	f.runs = append(f.runs, input)
	f.sessions = append(f.sessions, sessionID)
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	f.mu.Lock()
	f.events = append(f.events, "exit:"+input)
	err := f.err
	f.mu.Unlock()

	if err != nil {
		return "", err
	}
	return "回答:" + input, nil
}

func (f *fakeExecutor) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeExecutor) sessionList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sessions...)
}

func (f *fakeExecutor) eventList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// ===== 构造 =====

// testPipeline 构造一个放行一切门禁的管道。
func testPipeline(t *testing.T, s channel.Sender, ex Executor) *Pipeline {
	t.Helper()
	p, err := New(Config{
		Sender:   s,
		Executor: ex,
		Gate: GateConfig{
			Activation: channel.ActivationAlways,
			Audience:   channel.AudienceEveryone,
			BotOpenID:  "ou_bot",
		},
		Route: channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingSingleContext},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// directMsg 构造一条私聊消息。
func directMsg(text string) *channel.IncomingMessage {
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    "ou_user",
		ChatType:  channel.ChatDirect,
		MessageID: "om_1",
		Content:   text,
	}
}

// ===== AC-3：会话隔离（sessionID 必须按会话区分） =====

func TestPipeline_SessionIDIsPerConversation(t *testing.T) {
	// 真实平台验证暴露的缺陷（issue #9 Wave 6）：Executor 收到的 sessionID
	// 为空，trpc 报 "sessionID is required"，每条消息都失败。
	//
	// 修法不只是「给个非空值」——sessionID 必须是 **per-conversation** 的：
	// 同一会话的两条消息要共享历史（AC-3 后半），不同会话必须隔离。
	// 路由结果里的 effectiveJID 正是天然的会话键。
	sender := &fakeSender{}
	ex := &fakeExecutor{}
	p := testPipeline(t, sender, ex)

	// 同一会话两条
	for i := 0; i < 2; i++ {
		if err := p.Handle(context.Background(), directMsg("hi")); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	// 另一个会话一条（不同 chatID）
	other := directMsg("hi")
	other.UserID = "ou_another"
	if err := p.Handle(context.Background(), other); err != nil {
		t.Fatalf("Handle other: %v", err)
	}

	sessions := ex.sessionList()
	if len(sessions) != 3 {
		t.Fatalf("executor calls = %d, want 3", len(sessions))
	}
	// 非空
	for i, s := range sessions {
		if s == "" {
			t.Fatalf("call %d got empty sessionID — trpc would reject it", i)
		}
	}
	// 同会话共享
	if sessions[0] != sessions[1] {
		t.Errorf("same conversation must share a session: %q vs %q", sessions[0], sessions[1])
	}
	// 不同会话隔离
	if sessions[0] == sessions[2] {
		t.Errorf("different conversations must NOT share a session: both = %q", sessions[0])
	}
}

// ===== 正常路径：回答回到飞书（AC-1 的本地半程） =====

func TestPipeline_AnswersBackToFeishu(t *testing.T) {
	sender := &fakeSender{}
	ex := &fakeExecutor{}
	p := testPipeline(t, sender, ex)

	if err := p.Handle(context.Background(), directMsg("你好")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 执行了一次
	if n := ex.runCount(); n != 1 {
		t.Fatalf("executor runs = %d, want 1", n)
	}
	// 出站了——且是「占位 → 更新」两步（§4.4.1 的简化版流式）
	calls := sender.snapshot()
	if len(calls) != 2 {
		t.Fatalf("send calls = %d, want 2 (placeholder then update)", len(calls))
	}
	if calls[0].MessageID != "" {
		t.Error("first call must be a create (no message id)")
	}
	if calls[1].MessageID == "" {
		t.Error("second call must be an update (carry message id)")
	}
	if !strings.Contains(calls[1].Text, "回答:你好") {
		t.Errorf("final text = %q, want it to contain the answer", calls[1].Text)
	}
	// 私聊：接收者用 open_id 而非 chat_id
	if calls[0].IDType != channel.ReceiveIDOpen {
		t.Errorf("direct chat must send via open_id, got %q", calls[0].IDType)
	}
	if calls[0].To != "ou_user" {
		t.Errorf("receiver = %q, want ou_user", calls[0].To)
	}
}

func TestPipeline_GroupUsesChatID(t *testing.T) {
	sender := &fakeSender{}
	p := testPipeline(t, sender, &fakeExecutor{})

	msg := &channel.IncomingMessage{
		Platform: channel.PlatformFeishu,
		UserID:   "ou_user",
		ChatID:   "oc_group",
		ChatType: channel.ChatGroup,
		Content:  "hi",
	}
	if err := p.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	calls := sender.snapshot()
	if len(calls) == 0 {
		t.Fatal("no send calls")
	}
	if calls[0].IDType != channel.ReceiveIDChat {
		t.Errorf("group must send via chat_id, got %q", calls[0].IDType)
	}
	if calls[0].To != "oc_group" {
		t.Errorf("receiver = %q, want oc_group", calls[0].To)
	}
}

// ===== AC-4：门禁拒绝 → 零副作用（双向断言） =====

func TestPipeline_GateRejectHasNoSideEffects(t *testing.T) {
	cases := []struct {
		name string
		gate GateConfig
		msg  *channel.IncomingMessage
	}{
		{
			name: "群内未 @ bot",
			gate: GateConfig{
				Activation: channel.ActivationWhenMentioned,
				Audience:   channel.AudienceEveryone,
				BotOpenID:  "ou_bot",
			},
			msg: &channel.IncomingMessage{
				Platform: channel.PlatformFeishu, UserID: "ou_u",
				ChatID: "oc_g", ChatType: channel.ChatGroup, Content: "hi",
			},
		},
		{
			name: "disabled 硬停止",
			gate: GateConfig{Activation: channel.ActivationDisabled},
			msg:  directMsg("hi"),
		},
		{
			name: "botOpenID 未知（fail-closed）",
			gate: GateConfig{
				Activation: channel.ActivationWhenMentioned,
				BotOpenID:  "",
			},
			msg: &channel.IncomingMessage{
				Platform: channel.PlatformFeishu, UserID: "ou_u",
				ChatID: "oc_g", ChatType: channel.ChatGroup, Content: "hi",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &fakeSender{}
			ex := &fakeExecutor{}
			p, err := New(Config{
				Sender:   sender,
				Executor: ex,
				Gate:     tc.gate,
				Route:    channel.RouteConfig{WorkspaceID: "ws1"},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			if err := p.Handle(context.Background(), tc.msg); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			// **双向断言**：没有出站、也没有启动 run。
			// 只断言其一不足——「没发消息但跑了模型」仍是浪费与泄漏。
			if n := sender.count(); n != 0 {
				t.Errorf("send calls = %d, want 0 (rejected message must produce no reply)", n)
			}
			if n := ex.runCount(); n != 0 {
				t.Errorf("executor runs = %d, want 0 (rejected message must not start a run)", n)
			}
		})
	}
}

// ===== AC-3：同会话串行（不交错） =====

func TestPipeline_SerializesSameSession(t *testing.T) {
	sender := &fakeSender{}
	ex := &fakeExecutor{delay: 40 * time.Millisecond}
	p := testPipeline(t, sender, ex)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			if err := p.Handle(context.Background(), directMsg("m")); err != nil {
				t.Errorf("Handle: %v", err)
			}
		}()
	}
	wg.Wait()

	// 事件序列必须成对（enter:X 紧跟 exit:X）——交错说明串行化失效。
	ev := ex.eventList()
	if len(ev) != 4 {
		t.Fatalf("events = %v, want 4", ev)
	}
	for i := 0; i < len(ev); i += 2 {
		if !strings.HasPrefix(ev[i], "enter:") || !strings.HasPrefix(ev[i+1], "exit:") {
			t.Fatalf("session log interleaved: %v", ev)
		}
	}
}

// ===== 上下文降权：IM 来源以 channel kind 执行 =====

func TestPipeline_ExecutesWithChannelContext(t *testing.T) {
	// IM 来源必须降权为只读上下文（§4.3.3）。管道若不注入 kind，
	// 下游的 RequireWritable 会 fail-closed 拒绝——但那是「拒绝」而非
	// 「正确降权」，且会让每次写操作报 ErrContextKindMissing 而非
	// 语义正确的 ErrWriteDenied。
	sender := &fakeSender{}
	ex := &recordingExecutor{}
	p := testPipeline(t, sender, ex)

	if err := p.Handle(context.Background(), directMsg("hi")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if ex.kind != "channel" {
		t.Errorf("context kind = %q, want channel (IM must be downgraded)", ex.kind)
	}
}

// recordingExecutor 记录执行时看到的 context kind。
type recordingExecutor struct {
	kind string
}

func (r *recordingExecutor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	if k, ok := authz.ContextKindFrom(ctx); ok {
		r.kind = string(k)
	}
	return "ok", nil
}

// ===== 错误传播 =====

func TestPipeline_ExecutorErrorDoesNotSendAnswer(t *testing.T) {
	// 模型失败时不能往飞书发空消息——用户会看到一条空白回复。
	// 但占位消息已发出（两步流式的第一步），故应更新为错误提示而非留空。
	sender := &fakeSender{}
	ex := &fakeExecutor{err: errors.New("model down")}
	p := testPipeline(t, sender, ex)

	err := p.Handle(context.Background(), directMsg("hi"))
	if err == nil {
		t.Fatal("executor error must propagate")
	}
	calls := sender.snapshot()
	if len(calls) != 2 {
		t.Fatalf("send calls = %d, want 2 (placeholder + error notice)", len(calls))
	}
	if calls[1].Text == "" {
		t.Error("error notice must not be empty (user would see a blank reply)")
	}
}

func TestPipeline_SenderErrorPropagates(t *testing.T) {
	sender := &fakeSender{err: errors.New("feishu down")}
	p := testPipeline(t, sender, &fakeExecutor{})

	if err := p.Handle(context.Background(), directMsg("hi")); err == nil {
		t.Fatal("sender error must propagate")
	}
}

func TestPipeline_NilMessageRejected(t *testing.T) {
	p := testPipeline(t, &fakeSender{}, &fakeExecutor{})
	if err := p.Handle(context.Background(), nil); err == nil {
		t.Fatal("nil message must be rejected")
	}
}

// ===== 装配校验 =====

func TestNew_RequiresDependencies(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"缺 Sender", Config{Executor: &fakeExecutor{}}},
		{"缺 Executor", Config{Sender: &fakeSender{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Error("New must reject incomplete config")
			}
		})
	}
}
