package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// 单轮执行 API 测试（issue #9 Wave 2）。
//
// 为什么需要这个 API：CLI 的 Run 是**交互循环**（bufio.Scanner + exit/quit），
// 服务端（飞书 webhook/长连接）需要的是「给一句输入，拿完整回答」。
// 把循环与单轮拆开，两者才能各自复用同一套事件流语义（角色判定、
// 工具调用轮次、终止信号）。

// brokenSSE 返回一个总是失败（连接层拒绝）的端点。
func brokenSSE(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// newTestExecutor 装配一个测试用 Executor，并在测试结束时释放。
func newTestExecutor(t *testing.T, opts Options) *Executor {
	t.Helper()
	ex, err := NewExecutor(opts)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	t.Cleanup(ex.Close)
	return ex
}

// ===== Execute：聚合完整回答 =====

func TestExecute_ReturnsFullAnswer(t *testing.T) {
	// 服务端要一次性发送完整回答，不是逐块打印。
	srv, _ := mockSSE(t, []string{"你好", "，", "世界"})

	ex := newTestExecutor(t, testOptions(srv.URL, &sseRecorder{}))
	got, err := ex.Execute(context.Background(), "sess-1", "hi")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "你好，世界" {
		t.Errorf("answer = %q, want %q", got, "你好，世界")
	}
}

func TestExecutor_MultiTurnCarriesHistory(t *testing.T) {
	// 服务端的「第二条能看到第一条上下文」（AC-3 后半）依赖 session 承载历史。
	//
	// **这条测试曾经失败，暴露了一个真实的架构约束**：首版 Execute 是
	// 函数，每次调用都 newRunner → 新的 inmemory session service →
	// 同一 SessionID 的历史无法延续。探针证据：两次独立装配各跑一轮，
	// 模型收到的消息数是 [1 1]（历史丢失）；同一个 runner 连跑两轮是
	// [1 1 1 3]（第二轮 3 条，历史保留）。
	//
	// 因此 API 改为长驻的 Executor：装配一次，跨消息复用同一 runner。
	srv, msgCounts := mockSSE(t, []string{"ok"})

	ex := newTestExecutor(t, testOptions(srv.URL, &sseRecorder{}))
	for i := 0; i < 2; i++ {
		if _, err := ex.Execute(context.Background(), "sess-1", "问题"); err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
	}

	counts := *msgCounts
	if len(counts) < 2 {
		t.Fatalf("expected 2 model requests, got %d", len(counts))
	}
	// 第二轮请求的消息数必须多于第一轮——证明历史被带上。
	if counts[1] <= counts[0] {
		t.Errorf("second turn message count = %d, want > first turn %d (history must be carried)",
			counts[1], counts[0])
	}
}

func TestExecute_NonStreamFallback(t *testing.T) {
	// 非流式 provider 的整段内容在 Message.Content，Execute 也必须取到。
	srv, _ := mockSSE(t, []string{"整段回答"})

	opts := testOptions(srv.URL, &sseRecorder{})
	opts.ForceNonStream = true
	ex := newTestExecutor(t, opts)

	got, err := ex.Execute(context.Background(), "sess-1", "hi")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "整段回答" {
		t.Errorf("answer = %q, want %q", got, "整段回答")
	}
}

func TestExecute_EmptyInputRejected(t *testing.T) {
	// 空输入不应发起一次模型调用——飞书侧空消息会在门禁前被拦，
	// 但 API 自身也要 fail-fast，避免下游误用。
	srv, msgCounts := mockSSE(t, []string{"x"})

	ex := newTestExecutor(t, testOptions(srv.URL, &sseRecorder{}))
	_, err := ex.Execute(context.Background(), "sess-1", "   ")
	if err == nil {
		t.Fatal("blank input must be rejected")
	}
	if n := len(*msgCounts); n != 0 {
		t.Errorf("no model request should be made for blank input, got %d", n)
	}
}

func TestExecute_PropagatesModelError(t *testing.T) {
	// 模型错误必须作为 error 返回，不能静默变成空回答——
	// 否则服务端会往飞书发一条空消息。
	srv := brokenSSE(t)

	ex := newTestExecutor(t, testOptions(srv, &sseRecorder{}))
	_, err := ex.Execute(context.Background(), "sess-1", "hi")
	if err == nil {
		t.Fatal("model error must propagate, not become an empty answer")
	}
}

// ===== 角色判定：tool 角色不是正文（issue #3 的回归护栏） =====

func TestIsAssistantText_RoleJudgement(t *testing.T) {
	// 工具调用链中，tool 角色的消息承载工具返回值——它是给模型看的
	// 中间产物，不是给用户的回答。若不区分角色，工具结果会被当作正文
	// 混进最终回答（issue #3 实测发现的缺陷）。
	//
	// 这条测试是**变异测试驱动补上的**：把 isAssistantText 改成恒 true 后
	// 原有测试全部仍然通过——说明这条不变量此前零覆盖。
	//
	// 为什么直接测纯函数而非走 SSE：探针实测发现 trpc 的 openai provider
	// 会把流中所有 chunk 的 role 规范化成 assistant（构造 "tool" 角色的
	// delta 到了消费端已变成 assistant），因此**无法通过 SSE 端点模拟
	// tool 角色**。判定逻辑本身在 isAssistantText，直接测它才是有效覆盖。
	cases := []struct {
		role model.Role
		want bool
	}{
		{model.RoleAssistant, true},
		{"", true}, // 空角色放行：流式 delta 只在首块带 role，后续块 role 为空
		{model.RoleTool, false},
		{model.RoleUser, false},
		{model.RoleSystem, false},
	}
	for _, tc := range cases {
		if got := isAssistantText(tc.role); got != tc.want {
			t.Errorf("isAssistantText(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
}

// ===== Run 复用 Execute 后行为不变（回归） =====

func TestRun_StillStreamsInChunksAfterRefactor(t *testing.T) {
	// 重构 oneTurn 复用 Execute 后，CLI 仍须逐块输出（不能退化成一次性整段）。
	// 这条是防止「抽出 Execute 时把流式行为弄丢」的回归护栏。
	srv, _ := mockSSE(t, []string{"a", "b", "c"})

	out := &sseRecorder{}
	if err := Run(context.Background(), strings.NewReader("hi\nexit\n"), testOptions(srv.URL, out)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := out.chunkCount(); n < 3 {
		t.Errorf("chunk count = %d, want >=3 (CLI must stay streaming)", n)
	}
	if got := strings.TrimRight(out.String(), "\n"); got != "abc" {
		t.Errorf("output = %q, want abc", got)
	}
}
