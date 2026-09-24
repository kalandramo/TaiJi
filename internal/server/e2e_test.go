package server_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 端到端集成测试（issue #9）。
//
// 与各包单元测试的区别：这里串起**真实**的去重 → 门禁 → 路由 →
// 串行 → 执行 → 出站链路，只替换两处外部依赖：
//
//	飞书 API（出站）  → fakeSender（记录调用）
//	模型 provider     → fakeExecutor（返回固定回答）
//
// **驱动方式已从 webhook 改为直投 Dispatcher**：原型只用长连接，
// webhook 实现（HTTP 端点 + 验签 + 解析）已删除。长连接路径的真实形态是
// SDK 回调产出 IncomingMessage 后直接 Enqueue（见 longconn.go），
// 故这里直接构造该结构并投递——比用 webhook 解析更贴近实际。
//
// 随之删除的两条测试（其能力随 webhook 消失）：
//   - TestE2E_ForgedTokenProducesNoReply —— 验签是 webhook 专属边界；
//     长连接不验签（信任 SDK 与飞书的 TLS 通道，§2.2），无对应场景
//   - TestE2E_HTTPRespondsWithoutWaitingForAgent —— 异步性验证 HTTP 响应
//     不等 agent；无 HTTP 端点后该断言失去载体（异步性仍由 Enqueue 的
//     非阻塞语义保证，见 TestE2E_EnqueueDoesNotBlockOnAgent）
//
// 放在 server_test 外部测试包：需要同时 import channel 与 server，
// 而 server 本身不依赖具体渠道实现。

// recordingSender 记录出站调用。
type recordingSender struct {
	mu    sync.Mutex
	calls []sendCall
}

type sendCall struct {
	To        string
	Text      string
	IDType    channel.ReceiveIDType
	MessageID string
}

func (s *recordingSender) SendMessage(ctx context.Context, to, text string, opts channel.SendOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sendCall{To: to, Text: text, IDType: opts.ReceiveIDType, MessageID: opts.MessageID})
	if opts.MessageID != "" {
		return opts.MessageID, nil
	}
	return "om_placeholder", nil
}

func (s *recordingSender) snapshot() []sendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sendCall(nil), s.calls...)
}

// echoExecutor 返回可预测的回答。
type echoExecutor struct {
	mu   sync.Mutex
	seen []string
}

// ExecuteStream 满足 server.Executor 接口（issue #10）。
// 这些 fake 不测流式，故忽略 onChunk 直接委托 Execute。
func (e *echoExecutor) ExecuteStream(ctx context.Context, sessionID, input string, onChunk func(string)) (string, error) {
	return e.Execute(ctx, sessionID, input)
}

func (e *echoExecutor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	e.mu.Lock()
	e.seen = append(e.seen, input)
	e.mu.Unlock()
	return "回答:" + input, nil
}

func (e *echoExecutor) inputs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...)
}

// harness 是端到端测试夹具。
type harness struct {
	dispatcher *server.Dispatcher
	sender     *recordingSender
	executor   *echoExecutor
}

func newHarness(t *testing.T, gate server.GateConfig) *harness {
	t.Helper()
	sender := &recordingSender{}
	exec := &echoExecutor{}

	p, err := server.New(server.Config{
		Sender:   sender,
		Executor: exec,
		Gate:     gate,
		Route:    channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	d, err := server.NewDispatcher(server.DispatcherConfig{
		Handler: p,
		Deduper: channel.NewDeduper(time.Minute),
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	t.Cleanup(d.Stop)

	return &harness{dispatcher: d, sender: sender, executor: exec}
}

// send 投递一条消息（等价于长连接 SDK 回调的产出）。
func (h *harness) send(t *testing.T, msg *channel.IncomingMessage) {
	t.Helper()
	if err := h.dispatcher.Enqueue(msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

// waitCalls 等待出站调用数达到 n。
func (h *harness) waitCalls(t *testing.T, n int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(h.sender.snapshot()) >= n {
			return true
		}
		time.Sleep(3 * time.Millisecond)
	}
	return len(h.sender.snapshot()) >= n
}

// directMsg 构造私聊消息（长连接路径产物）。
func directMsg(messageID, text string) *channel.IncomingMessage {
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    "ou_sender",
		ChatID:    "",
		ChatType:  channel.ChatDirect,
		MessageID: messageID,
		Content:   text,
		Meta: &channel.ChannelMessageMeta{
			Provider:  string(channel.PlatformFeishu),
			ChatType:  "p2p",
			MessageID: messageID,
			Text:      text,
		},
	}
}

// groupMsg 构造群聊消息，可选 @bot。
func groupMsg(messageID, text string, botOpenID string, mention bool) *channel.IncomingMessage {
	var mentions []channel.Mention
	if mention {
		mentions = []channel.Mention{{OpenID: botOpenID, Key: "@_user_1", Name: "TaiJi"}}
	}
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    "ou_sender",
		ChatID:    "oc_group",
		ChatType:  channel.ChatGroup,
		MessageID: messageID,
		Content:   text,
		Mentions:  mentions,
		Meta: &channel.ChannelMessageMeta{
			Provider:  string(channel.PlatformFeishu),
			ChatType:  "group",
			MessageID: messageID,
			Text:      text,
		},
	}
}

func allowAllGate() server.GateConfig {
	return server.GateConfig{
		Activation: channel.ActivationAlways,
		Audience:   channel.AudienceEveryone,
		BotOpenID:  "ou_bot",
	}
}

// ===== AC-1：私聊 → 回答回到飞书 =====

func TestE2E_DirectMessageGetsReply(t *testing.T) {
	h := newHarness(t, allowAllGate())

	h.send(t, directMsg("om_1", "你好"))

	// 异步处理：等出站
	if !h.waitCalls(t, 2, 3*time.Second) {
		t.Fatalf("expected 2 send calls (placeholder + update), got %d", len(h.sender.snapshot()))
	}

	calls := h.sender.snapshot()
	if calls[0].Text == "" || calls[0].MessageID != "" {
		t.Errorf("first call must be a placeholder create, got %+v", calls[0])
	}
	if !contains(calls[1].Text, "回答:你好") {
		t.Errorf("final text = %q, want the answer", calls[1].Text)
	}
	if calls[1].MessageID != "om_placeholder" {
		t.Errorf("second call must update the placeholder, got id=%q", calls[1].MessageID)
	}
	// 私聊用 open_id
	if calls[0].IDType != channel.ReceiveIDOpen {
		t.Errorf("direct chat must reply via open_id, got %q", calls[0].IDType)
	}
	// 执行器收到了用户输入
	if in := h.executor.inputs(); len(in) != 1 || in[0] != "你好" {
		t.Errorf("executor inputs = %v, want [你好]", in)
	}
}

// ===== AC-4：群内未 @ bot → 静默丢弃 =====

func TestE2E_GroupWithoutMentionIsSilentlyDropped(t *testing.T) {
	h := newHarness(t, server.GateConfig{
		Activation: channel.ActivationWhenMentioned,
		Audience:   channel.AudienceEveryone,
		BotOpenID:  "ou_bot",
	})

	// 群里发言但不 @ bot
	h.send(t, groupMsg("om_2", "大家好", "ou_bot", false))

	// 给足时间：不应有任何出站、也没有 run
	time.Sleep(200 * time.Millisecond)
	if n := len(h.sender.snapshot()); n != 0 {
		t.Errorf("send calls = %d, want 0 (未 @ bot 必须静默丢弃)", n)
	}
	if n := len(h.executor.inputs()); n != 0 {
		t.Errorf("executor runs = %d, want 0", n)
	}
}

func TestE2E_GroupWithMentionIsAnswered(t *testing.T) {
	// 对照：@ 了 bot 就应回答——证明上一条的「丢弃」来自 @ 判定，
	// 而不是群聊路径整体坏了。
	h := newHarness(t, server.GateConfig{
		Activation: channel.ActivationWhenMentioned,
		Audience:   channel.AudienceEveryone,
		BotOpenID:  "ou_bot",
	})

	h.send(t, groupMsg("om_3", "@_user_1 你好", "ou_bot", true))
	if !h.waitCalls(t, 2, 3*time.Second) {
		t.Fatalf("expected a reply for a mentioned group message, got %d calls", len(h.sender.snapshot()))
	}
	// 群聊用 chat_id
	if calls := h.sender.snapshot(); calls[0].IDType != channel.ReceiveIDChat {
		t.Errorf("group must reply via chat_id, got %q", calls[0].IDType)
	}
}

// ===== AC-3：同会话两条 → 顺序处理且带上下文 =====

func TestE2E_SequentialMessagesInSameSession(t *testing.T) {
	h := newHarness(t, allowAllGate())

	// 连发两条（同一私聊会话）
	h.send(t, directMsg("om_5", "第一句"))
	h.send(t, directMsg("om_6", "第二句"))

	// 两条都应被处理（异步 + 串行）
	if !h.waitCalls(t, 4, 5*time.Second) {
		t.Fatalf("expected 4 send calls (2 messages × 2 steps), got %d", len(h.sender.snapshot()))
	}

	inputs := h.executor.inputs()
	if len(inputs) != 2 {
		t.Fatalf("executor runs = %d, want 2", len(inputs))
	}
	// 顺序必须与投递顺序一致（串行化的语义）
	if inputs[0] != "第一句" || inputs[1] != "第二句" {
		t.Errorf("execution order = %v, want [第一句 第二句]", inputs)
	}
}

// ===== 去重：同一 message_id 重投只处理一次 =====

func TestE2E_DuplicateDeliveryHandledOnce(t *testing.T) {
	h := newHarness(t, allowAllGate())

	for i := 0; i < 3; i++ {
		h.send(t, directMsg("om_dup", "重复投递"))
	}

	if !h.waitCalls(t, 2, 3*time.Second) {
		t.Fatalf("expected 2 send calls, got %d", len(h.sender.snapshot()))
	}
	// 给足时间暴露重复
	time.Sleep(250 * time.Millisecond)

	if n := len(h.executor.inputs()); n != 1 {
		t.Errorf("executor runs = %d, want 1 (重复投递必须被去重)", n)
	}
	if n := len(h.sender.snapshot()); n != 2 {
		t.Errorf("send calls = %d, want 2 (one placeholder + one update)", n)
	}
}

// ===== 异步性：Enqueue 不阻塞调用方（agent 跑得慢也不影响）=====

func TestE2E_EnqueueDoesNotBlockOnAgent(t *testing.T) {
	// 原测试验证「HTTP 响应不等 agent」；无 HTTP 端点后，等价的断言是
	// 「Enqueue 不等 agent」——它是长连接路径的实际调用点
	// （longconn.go 的 SDK 回调里调 Enqueue）。
	sender := &recordingSender{}
	slow := &slowExecutor{delay: 800 * time.Millisecond}

	p, err := server.New(server.Config{
		Sender:   sender,
		Executor: slow,
		Gate:     allowAllGate(),
		Route:    channel.RouteConfig{WorkspaceID: "ws1"},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	d, err := server.NewDispatcher(server.DispatcherConfig{Handler: p})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Stop()

	start := time.Now()
	if err := d.Enqueue(directMsg("om_slow", "慢问题")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	elapsed := time.Since(start)

	// 必须远快于执行时间（800ms）。留足余量：< 300ms 即证明未等待。
	if elapsed > 300*time.Millisecond {
		t.Errorf("Enqueue took %v, want < 300ms (must not block on the agent)", elapsed)
	}
}

type slowExecutor struct{ delay time.Duration }

// ExecuteStream 满足 server.Executor 接口（issue #10）。
// 这些 fake 不测流式，故忽略 onChunk 直接委托 Execute。
func (s *slowExecutor) ExecuteStream(ctx context.Context, sessionID, input string, onChunk func(string)) (string, error) {
	return s.Execute(ctx, sessionID, input)
}

func (s *slowExecutor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "慢回答", nil
}

// ===== 两种投递路径的下游行为一致 =====

func TestE2E_IngressPathsShareDownstreamBehaviour(t *testing.T) {
	// 长连接是唯一入站路径，但其消息可能来自不同形态（群聊/私聊、
	// 带/不带 mention）。这里断言「形状不同 → 下游语义不漂移」：
	// 若归一化漏了 chat_type，群聊的 @ 门禁就会失效。
	h := newHarness(t, server.GateConfig{
		Activation: channel.ActivationWhenMentioned,
		BotOpenID:  "ou_bot",
	})

	// 群聊未 @ → 丢弃
	h.send(t, groupMsg("om_g", "hi", "ou_bot", false))
	time.Sleep(120 * time.Millisecond)

	// 同形状但 @ 了 bot → 应答
	h.send(t, groupMsg("om_g2", "@bot hi", "ou_bot", true))
	if !h.waitCalls(t, 2, 3*time.Second) {
		t.Fatalf("mentioned message must be answered, got %d calls", len(h.sender.snapshot()))
	}

	// 只应有一次应答（未 @ 的那条被丢弃）
	if n := len(h.executor.inputs()); n != 1 {
		t.Errorf("executor runs = %d, want 1 (only the mentioned message)", n)
	}
}

// contains 是包内测试辅助。
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
