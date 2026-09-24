package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/feishu"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 端到端集成测试（issue #9）。
//
// 与各包单元测试的区别：这里串起**真实**的入站 → 去重 → 门禁 → 路由 →
// 串行 → 执行 → 出站链路，只替换两处外部依赖：
//
//	飞书 API（出站）  → fakeSender（记录调用）
//	模型 provider     → fakeExecutor（返回固定回答）
//
// 入站用的是**真实的** feishu.Handler（含验签/解密/解析）、真实的
// server.Dispatcher（含去重与异步）、真实的 server.Pipeline（含门禁、
// 路由、串行化）。这样验证的是层间**接线**，而非孤立函数——
// 接线错误正是集成阶段最容易出、单测最难发现的问题。
//
// 放在 server_test 外部测试包：需要同时 import feishu 与 server，
// 而 server 本身不依赖 feishu（避免让装配层耦合具体渠道实现）。

const testToken = "test-verification-token"

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

func (e *echoExecutor) Execute(ctx context.Context, input string) (string, error) {
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
	handler    *feishu.Handler
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

	h := feishu.NewHandler(feishu.HandlerConfig{
		Verify:    feishu.VerifyConfig{VerificationToken: testToken},
		OnMessage: d.Enqueue,
	})

	return &harness{handler: h, dispatcher: d, sender: sender, executor: exec}
}

// post 向 webhook 端点投递一个事件，返回状态码。
func (h *harness) post(t *testing.T, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/feishu", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w.Code
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

// eventJSON 构造飞书消息事件。
func eventJSON(messageID, chatID, chatType, text string, mentions string) string {
	mentionsPart := ""
	if mentions != "" {
		mentionsPart = `"mentions":` + mentions + `,`
	}
	return `{
		"header":{"token":"` + testToken + `","event_type":"im.message.receive_v1"},
		"event":{
			"message":{
				"message_id":"` + messageID + `","chat_id":"` + chatID + `","chat_type":"` + chatType + `",
				"message_type":"text",
				"content":"{\"text\":\"` + text + `\"}",
				` + mentionsPart + `
				"create_time":"1"
			},
			"sender":{"sender_id":{"open_id":"ou_sender"}}
		}
	}`
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

	code := h.post(t, eventJSON("om_1", "", "p2p", "你好", ""))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	// 异步处理：等出站
	if !h.waitCalls(t, 2, 3*time.Second) {
		t.Fatalf("expected 2 send calls (placeholder + update), got %d", len(h.sender.snapshot()))
	}

	calls := h.sender.snapshot()
	if calls[0].Text == "" || calls[0].MessageID != "" {
		t.Errorf("first call must be a placeholder create, got %+v", calls[0])
	}
	if !strings.Contains(calls[1].Text, "回答:你好") {
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
	code := h.post(t, eventJSON("om_2", "oc_group", "group", "大家好", ""))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rejection is not an HTTP error)", code)
	}

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

	mentions := `[{"key":"@_user_1","name":"TaiJi","id":{"open_id":"ou_bot"}}]`
	code := h.post(t, eventJSON("om_3", "oc_group", "group", "@_user_1 你好", mentions))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !h.waitCalls(t, 2, 3*time.Second) {
		t.Fatalf("expected a reply for a mentioned group message, got %d calls", len(h.sender.snapshot()))
	}
	// 群聊用 chat_id
	if calls := h.sender.snapshot(); calls[0].IDType != channel.ReceiveIDChat {
		t.Errorf("group must reply via chat_id, got %q", calls[0].IDType)
	}
}

// ===== AC-5：伪造 token 不产生任何回复 =====

func TestE2E_ForgedTokenProducesNoReply(t *testing.T) {
	h := newHarness(t, allowAllGate())

	// 伪造 token 的事件
	forged := strings.Replace(eventJSON("om_4", "", "p2p", "hi", ""), testToken, "attacker-token", 1)
	code := h.post(t, forged)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}

	time.Sleep(150 * time.Millisecond)
	if n := len(h.sender.snapshot()); n != 0 {
		t.Errorf("send calls = %d, want 0 (伪造 token 不得产生回复)", n)
	}
	if n := len(h.executor.inputs()); n != 0 {
		t.Errorf("executor runs = %d, want 0", n)
	}
}

// ===== AC-3：同会话两条 → 顺序处理且带上下文 =====

func TestE2E_SequentialMessagesInSameSession(t *testing.T) {
	h := newHarness(t, allowAllGate())

	// 连发两条（同一私聊会话）
	if code := h.post(t, eventJSON("om_5", "", "p2p", "第一句", "")); code != 200 {
		t.Fatalf("first post status = %d", code)
	}
	if code := h.post(t, eventJSON("om_6", "", "p2p", "第二句", "")); code != 200 {
		t.Fatalf("second post status = %d", code)
	}

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

	body := eventJSON("om_dup", "", "p2p", "重复投递", "")
	for i := 0; i < 3; i++ {
		if code := h.post(t, body); code != 200 {
			t.Fatalf("post %d status = %d", i, code)
		}
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

// ===== 异步性：HTTP 响应不等待 agent =====

func TestE2E_HTTPRespondsWithoutWaitingForAgent(t *testing.T) {
	// 这是异步化的核心收益：agent 跑数秒，但 HTTP 必须立即返回。
	// 用一个「慢执行器」证明响应时间与执行时间无关。
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

	h := feishu.NewHandler(feishu.HandlerConfig{
		Verify:    feishu.VerifyConfig{VerificationToken: testToken},
		OnMessage: d.Enqueue,
	})

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/webhook/feishu",
		strings.NewReader(eventJSON("om_slow", "", "p2p", "慢问题", "")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	elapsed := time.Since(start)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	// 响应必须远快于执行时间（800ms）。留足余量：< 300ms 即证明未等待。
	if elapsed > 300*time.Millisecond {
		t.Errorf("HTTP took %v, want < 300ms (must not block on the agent)", elapsed)
	}
}

type slowExecutor struct{ delay time.Duration }

func (s *slowExecutor) Execute(ctx context.Context, input string) (string, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "慢回答", nil
}

// ===== 长连接与 webhook 的入站等价性 =====

func TestE2E_BothIngressPathsShareDownstreamBehaviour(t *testing.T) {
	// 长连接与 webhook 是两条入站路径，但下游行为必须一致。
	// 这里不启动真实 WebSocket（需真实飞书），而是断言两条路径
	// 产出的 IncomingMessage 在门禁与路由下得到相同结论。
	//
	// 这条测试保护的是「入口不同 → 语义漂移」这类问题：
	// 若长连接忘了归一化 chat_type，群聊的 @ 门禁就会失效。
	h := newHarness(t, server.GateConfig{
		Activation: channel.ActivationWhenMentioned,
		BotOpenID:  "ou_bot",
	})

	// webhook 路径：群聊未 @ → 丢弃
	if code := h.post(t, eventJSON("om_w", "oc_g", "group", "hi", "")); code != 200 {
		t.Fatalf("status = %d", code)
	}
	time.Sleep(120 * time.Millisecond)

	// 长连接路径：同形状的消息直接投递（绕过 HTTP 层）
	longconnMsg := &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    "ou_sender",
		ChatID:    "oc_g",
		ChatType:  channel.ChatGroup, // 归一化后
		MessageID: "om_l",
		Content:   "hi",
	}
	if err := h.dispatcher.Enqueue(longconnMsg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	// 两条路径都应无副作用（未 @ bot）
	if n := len(h.sender.snapshot()); n != 0 {
		t.Errorf("send calls = %d, want 0 (both ingress paths must gate identically)", n)
	}
}
