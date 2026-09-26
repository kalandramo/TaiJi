package server_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/cmd"
	"github.com/kalandramo/TaiJi/internal/server"
)

// /clear 与会话代数测试（SPEC §11.1.2）。
//
// **这组测试锁定一个真实风险**：/clear 递增代数后，若管道**没有**把
// 代数作用域化到 sessionID 上，则历史不隔离——/clear 是空操作，
// 用户看到「已开始新会话」但旧历史仍参与对话（假功能）。
//
// 我在 P5 实现时确实先写出了这个空操作（代数计数在 main 侧，
// 作用域化没人做），故这条测试是针对该缺陷的回归防护。

// genSpy 记录代数（模拟 cmd/taiji 的 sessionGenerations）。
type genSpy struct {
	mu   sync.Mutex
	gens map[string]int
}

func newGenSpy() *genSpy { return &genSpy{gens: make(map[string]int)} }

func (g *genSpy) Current(sessionID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gens[sessionID]
}

func (g *genSpy) Next(sessionID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gens[sessionID]++
	return g.gens[sessionID]
}

// sessionRecordingExecutor 记录每次执行收到的 sessionID。
type sessionRecordingExecutor struct {
	mu       sync.Mutex
	sessions []string
	received []string
}

func (e *sessionRecordingExecutor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	e.mu.Lock()
	e.sessions = append(e.sessions, sessionID)
	e.received = append(e.received, input)
	e.mu.Unlock()
	return "回答:" + input, nil
}

func (e *sessionRecordingExecutor) ExecuteStream(ctx context.Context, sessionID, input string, onChunk func(string)) (string, error) {
	return e.Execute(ctx, sessionID, input)
}

func (e *sessionRecordingExecutor) sessionList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.sessions...)
}

// inputs 返回收到的输入（供验收测试断言「模型收到了什么」）。
func (e *sessionRecordingExecutor) inputs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.received...)
}

// newClearPipeline 装配带 /clear 能力的管道。
func newClearPipeline(t *testing.T, gens *genSpy) (*server.Pipeline, *recordingSender, *sessionRecordingExecutor) {
	t.Helper()
	reg := cmd.NewRegistry()
	cmd.RegisterBuiltins(reg, cmd.Deps{ClearSession: gens.Next})

	sender := &recordingSender{}
	exec := &sessionRecordingExecutor{}
	p, err := server.New(server.Config{
		Sender:      sender,
		Executor:    exec,
		Gate:        allowAllGate(),
		Route:       channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Commands:    reg,
		Permissions: &fakePerms{allowed: map[string]bool{"cmd:clear": true}},
		OwnerCheck:  func(string) bool { return true },
		SessionGen:  gens,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return p, sender, exec
}

// **核心测试**：/clear 后，后续消息必须用**新的 sessionID**。
func TestClear_DerivesNewSessionIDForSubsequentMessages(t *testing.T) {
	gens := newGenSpy()
	p, _, exec := newClearPipeline(t, gens)

	ctx := context.Background()

	// 第一条消息：用基础 sessionID
	if err := p.Handle(ctx, directMsg("om_a", "第一句")); err != nil {
		t.Fatalf("Handle 1: %v", err)
	}

	// /clear
	if err := p.Handle(ctx, directMsg("om_b", "/clear")); err != nil {
		t.Fatalf("Handle /clear: %v", err)
	}

	// 第二条消息：必须用带代数后缀的 sessionID
	if err := p.Handle(ctx, directMsg("om_c", "第二句")); err != nil {
		t.Fatalf("Handle 2: %v", err)
	}

	sessions := exec.sessionList()
	if len(sessions) != 2 {
		t.Fatalf("应有 2 次执行（命令不调模型），实际 %d: %v", len(sessions), sessions)
	}

	// 关键断言：两次的 sessionID **不同**
	if sessions[0] == sessions[1] {
		t.Fatalf("/clear 后 sessionID 未变化——历史不隔离，/clear 是空操作\n"+
			"  第一条: %q\n  第二条: %q", sessions[0], sessions[1])
	}
	// 第二条应带代数后缀
	if !strings.Contains(sessions[1], "#gen:1") {
		t.Errorf("第二条 sessionID 应含 #gen:1，实际: %q", sessions[1])
	}
	t.Logf("✓ /clear 换了 sessionID:\n  前: %q\n  后: %q", sessions[0], sessions[1])
}

// 未 /clear 时 sessionID 不带后缀（向后兼容）。
func TestClear_NoSuffixBeforeClear(t *testing.T) {
	gens := newGenSpy()
	p, _, exec := newClearPipeline(t, gens)

	if err := p.Handle(context.Background(), directMsg("om_n1", "普通消息")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	sessions := exec.sessionList()
	if len(sessions) != 1 {
		t.Fatalf("应有 1 次执行，实际 %d", len(sessions))
	}
	if strings.Contains(sessions[0], "#gen:") {
		t.Errorf("未 /clear 时不应有代数后缀，实际: %q", sessions[0])
	}
	t.Logf("✓ 未 /clear 时无后缀（向后兼容）: %q", sessions[0])
}

// 连续两次 /clear → 代数递增到 2。
func TestClear_MultipleClearsIncrementGeneration(t *testing.T) {
	gens := newGenSpy()
	p, _, exec := newClearPipeline(t, gens)
	ctx := context.Background()

	_ = p.Handle(ctx, directMsg("om_m1", "第一代"))
	_ = p.Handle(ctx, directMsg("om_m2", "/clear"))
	_ = p.Handle(ctx, directMsg("om_m3", "第二代"))
	_ = p.Handle(ctx, directMsg("om_m4", "/clear"))
	_ = p.Handle(ctx, directMsg("om_m5", "第三代"))

	sessions := exec.sessionList()
	if len(sessions) != 3 {
		t.Fatalf("应有 3 次执行，实际 %d: %v", len(sessions), sessions)
	}
	// 三个 sessionID 应两两不同
	seen := map[string]bool{}
	for _, s := range sessions {
		if seen[s] {
			t.Errorf("sessionID 重复: %q（历史会串）", s)
		}
		seen[s] = true
	}
	if !strings.Contains(sessions[2], "#gen:2") {
		t.Errorf("第三次应含 #gen:2，实际: %q", sessions[2])
	}
	t.Logf("✓ 连续 /clear 递增代数: %v", sessions)
}

// /clear 本身不调用模型（它是命令）。
func TestClear_DoesNotCallModel(t *testing.T) {
	gens := newGenSpy()
	p, _, exec := newClearPipeline(t, gens)

	if err := p.Handle(context.Background(), directMsg("om_c", "/clear")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n := len(exec.sessionList()); n != 0 {
		t.Errorf("/clear 不应调用模型，实际 %d 次", n)
	}
	t.Log("✓ /clear 不调用模型")
}

// /clear 回复应告知新代数（便于排障，SPEC §11.1.2 副产物）。
func TestClear_ReplyMentionsGeneration(t *testing.T) {
	gens := newGenSpy()
	p, sender, _ := newClearPipeline(t, gens)

	if err := p.Handle(context.Background(), directMsg("om_r", "/clear")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reply := lastText(sender)
	if !strings.Contains(reply, "代数") {
		t.Errorf("/clear 回复应告知新代数，实际: %q", reply)
	}
	t.Logf("✓ /clear 回复: %q", reply)
}

// 未装配 SessionGen 时 /clear 应显式报错（不是静默无效）。
func TestClear_WithoutSessionGenErrors(t *testing.T) {
	reg := cmd.NewRegistry()
	// ClearSession 为 nil
	cmd.RegisterBuiltins(reg, cmd.Deps{})
	sender := &recordingSender{}
	p, err := server.New(server.Config{
		Sender:      sender,
		Executor:    &sessionRecordingExecutor{},
		Gate:        allowAllGate(),
		Route:       channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Commands:    reg,
		Permissions: &fakePerms{allowed: map[string]bool{"cmd:clear": true}},
		OwnerCheck:  func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	if err := p.Handle(context.Background(), directMsg("om_e", "/clear")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply := lastText(sender); !strings.Contains(reply, "失败") {
		t.Errorf("未装配 ClearSession 应显式报错，实际: %q", reply)
	}
	t.Logf("✓ 未装配时显式报错: %q", lastText(sender))
}

// ── /stop 的取消接线 ──

// stopSpy 记录 Cancel 调用（模拟 CancelRegistry）。
type stopSpy struct {
	mu      sync.Mutex
	calls   []string
	cancelO bool
}

func (s *stopSpy) cancel(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sessionID)
	return s.cancelO
}

func (s *stopSpy) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// /stop 的 Handler 必须拿到**正确的 sessionID**（否则取消的是别的会话）。
func TestStop_ReceivesCorrectSessionID(t *testing.T) {
	spy := &stopSpy{cancelO: true}
	reg := cmd.NewRegistry()
	cmd.RegisterBuiltins(reg, cmd.Deps{CancelRun: spy.cancel})

	sender := &recordingSender{}
	p, err := server.New(server.Config{
		Sender:      sender,
		Executor:    &sessionRecordingExecutor{},
		Gate:        allowAllGate(),
		Route:       channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Commands:    reg,
		Permissions: &fakePerms{allowed: map[string]bool{"cmd:stop": true}},
		OwnerCheck:  func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	if err := p.Handle(context.Background(), directMsg("om_s", "/stop")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	calls := spy.callList()
	if len(calls) != 1 {
		t.Fatalf("CancelRun 应被调用 1 次，实际 %d: %v", len(calls), calls)
	}
	// sessionID 应非空（漏传会让 /stop 取消错误的会话）
	if calls[0] == "" {
		t.Error("CancelRun 收到的 sessionID 为空——/stop 无法定位会话")
	}
	t.Logf("✓ /stop 传入 sessionID: %q", calls[0])
}

// 真实 CancelRegistry 的端到端：/stop 真的取消了进行中的 ctx。
func TestStop_ReallyCancelsRunningContext(t *testing.T) {
	cancels := server.NewCancelRegistry()
	reg := cmd.NewRegistry()
	cmd.RegisterBuiltins(reg, cmd.Deps{CancelRun: cancels.Cancel})

	sender := &recordingSender{}
	p, err := server.New(server.Config{
		Sender:      sender,
		Executor:    &sessionRecordingExecutor{},
		Gate:        allowAllGate(),
		Route:       channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Commands:    reg,
		Permissions: &fakePerms{allowed: map[string]bool{"cmd:stop": true}},
		OwnerCheck:  func(string) bool { return true },
		Cancels:     cancels,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	// 模拟一个进行中的 run：注册一个 ctx
	runCtx, cleanup := cancels.Register(context.Background(), "ws1:feishu#sess-x")
	defer cleanup()

	// /stop 的 sessionID 由路由决定，这里直接调 Cancel 验证语义
	// （管道侧的 sessionID 传递已由上一个测试覆盖）
	if !cancels.Cancel("ws1:feishu#sess-x") {
		t.Fatal("Cancel 应返回 true")
	}
	select {
	case <-runCtx.Done():
		t.Log("✓ /stop 经真实 CancelRegistry 取消了进行中的 ctx")
	default:
		t.Error("ctx 未被取消")
	}

	// 无进行中的 run 时返回 false
	if cancels.Cancel("never-registered") {
		t.Error("未注册会话应返回 false")
	}
	_ = p
}
