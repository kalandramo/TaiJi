package cmd

import (
	"strings"
	"testing"
)

// 内置命令的测试（SPEC §4.1）。
//
// 这些测试**不需要 fake Sender**——Handler 只返回文本，不出站。
// 这是把出站移到调用方的直接收益。

func newBuiltinRegistry(t *testing.T) (*Registry, *depsSpy) {
	t.Helper()
	spy := &depsSpy{}
	r := NewRegistry()
	RegisterBuiltins(r, Deps{
		ClearSession: spy.clearSession,
		CancelRun:    spy.cancelRun,
	})
	return r, spy
}

// depsSpy 记录 Deps 回调的调用。
type depsSpy struct {
	clearCalls  []string
	cancelCalls []string
	nextGen     int
	cancelOK    bool
}

func (s *depsSpy) clearSession(sessionID string) int {
	s.clearCalls = append(s.clearCalls, sessionID)
	s.nextGen++
	return s.nextGen
}

func (s *depsSpy) cancelRun(sessionID string) bool {
	s.cancelCalls = append(s.cancelCalls, sessionID)
	return s.cancelOK
}

// ── 注册完整性 ──

func TestBuiltins_RegistersExactlyFourCommands(t *testing.T) {
	r, _ := newBuiltinRegistry(t)

	got := r.SortedNames()
	want := []string{"clear", "help", "status", "stop"}
	if len(got) != len(want) {
		t.Fatalf("注册命令数 = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SortedNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	t.Logf("✓ 只注册 4 条 v1 命令: %v", got)
}

// v2 命令不应被注册（SPEC §11.1.1：移出 v1）。
func TestBuiltins_V2CommandsNotRegistered(t *testing.T) {
	r, _ := newBuiltinRegistry(t)

	for _, name := range []string{"owner_mention", "release_owner"} {
		if _, ok := r.Lookup(name); ok {
			t.Errorf("%s 应在 v1 中未注册（需 owner 持久化）", name)
		}
	}
	t.Log("✓ /owner_mention 与 /release_owner 未注册（v2）")
}

// OwnerOnly 标记正确（SPEC §4.1）。
func TestBuiltins_OwnerOnlyFlags(t *testing.T) {
	r, _ := newBuiltinRegistry(t)

	wantOwnerOnly := map[string]bool{
		"help":   false,
		"status": false,
		"clear":  true,
		"stop":   true,
	}
	for name, want := range wantOwnerOnly {
		c, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("%s 未注册", name)
		}
		if c.OwnerOnly != want {
			t.Errorf("%s.OwnerOnly = %v, want %v", name, c.OwnerOnly, want)
		}
	}
	t.Log("✓ OwnerOnly 标记正确（clear/stop 需 owner）")
}

// ── /help ──

func TestBuiltin_HelpReturnsRegistryText(t *testing.T) {
	r, _ := newBuiltinRegistry(t)
	c, _ := r.Lookup("help")

	// AC-C2：/help 不调用模型——它只返回注册表文本（本测试证明不依赖任何外部）
	got, err := c.Handler(Request{Args: "ignored"})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	for _, n := range []string{"/help", "/status", "/clear", "/stop"} {
		if !strings.Contains(got, n) {
			t.Errorf("/help 输出应含 %s，实际:\n%s", n, got)
		}
	}
	t.Logf("✓ /help 返回 4 条命令（忽略 args）")
}

// ── /status ──

func TestBuiltin_StatusShowsSessionInfo(t *testing.T) {
	r, _ := newBuiltinRegistry(t)
	c, _ := r.Lookup("status")

	got, err := c.Handler(Request{
		SessionID:    "feishu:default#oc_abc",
		SessionGen:   2,
		QueuePending: 3,
		WorkspaceID:  "default",
	})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	for _, want := range []string{"feishu:default#oc_abc", "代数", "待处理消息", "default"} {
		if !strings.Contains(got, want) {
			t.Errorf("/status 输出应含 %q，实际:\n%s", want, got)
		}
	}
	t.Logf("✓ /status 展示会话信息")
}

// ── /clear ──

func TestBuiltin_ClearDerivesNewGeneration(t *testing.T) {
	r, spy := newBuiltinRegistry(t)
	c, _ := r.Lookup("clear")

	got, err := c.Handler(Request{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if len(spy.clearCalls) != 1 || spy.clearCalls[0] != "sess-1" {
		t.Errorf("ClearSession 应以 sessionID 调用一次，实际: %v", spy.clearCalls)
	}
	if !strings.Contains(got, "代数") {
		t.Errorf("回复应告知新代数（便于排障），实际: %q", got)
	}
	t.Logf("✓ /clear 触发 ClearSession 并告知代数: %q", got)
}

// 未装配 Deps 时应显式报错，而非静默无效。
func TestBuiltin_ClearWithoutDepsErrors(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, Deps{}) // ClearSession 为 nil
	c, _ := r.Lookup("clear")

	if _, err := c.Handler(Request{SessionID: "s"}); err == nil {
		t.Error("未装配 ClearSession 时应报错，而非静默返回成功")
	} else {
		t.Logf("✓ 未装配时显式报错: %v", err)
	}
}

// ── /stop ──

func TestBuiltin_StopCancelsRunningGeneration(t *testing.T) {
	r, spy := newBuiltinRegistry(t)
	spy.cancelOK = true // 模拟有进行中的 run
	c, _ := r.Lookup("stop")

	got, err := c.Handler(Request{SessionID: "sess-2"})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if len(spy.cancelCalls) != 1 || spy.cancelCalls[0] != "sess-2" {
		t.Errorf("CancelRun 应以 sessionID 调用一次，实际: %v", spy.cancelCalls)
	}
	if !strings.Contains(got, "已中断") {
		t.Errorf("成功中断的回复应说明已中断，实际: %q", got)
	}
	t.Logf("✓ /stop 中断成功: %q", got)
}

// SPEC §5.4 边界：无进行中的 run 时 /stop **不是错误**。
func TestBuiltin_StopWithNoRunningGeneration(t *testing.T) {
	r, spy := newBuiltinRegistry(t)
	spy.cancelOK = false // 模拟无进行中的 run
	c, _ := r.Lookup("stop")

	got, err := c.Handler(Request{SessionID: "sess-3"})
	if err != nil {
		t.Fatalf("无进行中的 run 不应报错，实际: %v", err)
	}
	if !strings.Contains(got, "没有") {
		t.Errorf("应说明没有进行中的生成，实际: %q", got)
	}
	t.Logf("✓ 无 run 时 /stop 正常回复: %q", got)
}

func TestBuiltin_StopWithoutDepsErrors(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r, Deps{}) // CancelRun 为 nil
	c, _ := r.Lookup("stop")

	if _, err := c.Handler(Request{SessionID: "s"}); err == nil {
		t.Error("未装配 CancelRun 时应报错")
	}
}

// 命令的 Handler 都不应依赖 args（v1 的 4 条都不消费参数，SPEC §7.2）。
func TestBuiltins_AllIgnoreArgs(t *testing.T) {
	r, spy := newBuiltinRegistry(t)
	spy.cancelOK = true

	for _, name := range []string{"help", "status", "clear", "stop"} {
		c, _ := r.Lookup(name)
		if _, err := c.Handler(Request{Args: "some extra args", SessionID: "s"}); err != nil {
			t.Errorf("%s 带 args 时不应报错: %v", name, err)
		}
	}
	t.Log("✓ 4 条命令均忽略 args（v1 不消费参数）")
}
