package server_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/cmd"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 命令接线测试（SPEC §9.4 AC-C1/C2/C4/C5/C7、AC-D3）。
//
// 这些测试走**完整 Pipeline.Handle 路径**（门禁 → 路由 → 命令 → 出站），
// 只有 Sender 与 Executor 是 fake——Registry 与 PermissionSource 是真的。
//
// 复用 e2e_test.go 的夹具（recordingSender / echoExecutor / directMsg /
// groupMsg / allowAllGate），不重造——重造会导致同包重复定义。

// ── 测试夹具（仅本文件需要的）──

// fakePerms 是可控的 PermissionSource（AC-D2：换实现不改消费方）。
//
// 它证明「命令只依赖 PermissionSource 接口」——用 fake 即可驱动，
// 无需真实 RBAC。
type fakePerms struct {
	allowed map[string]bool
	err     error
}

func (f *fakePerms) Allowed(_ context.Context, req authz.AccessRequest) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.allowed[req.Action], nil
}

// newCmdPipeline 装配带命令功能的管道。
func newCmdPipeline(
	t *testing.T,
	perms authz.PermissionSource,
	ownerCheck func(string) bool,
) (*server.Pipeline, *recordingSender, *echoExecutor) {
	t.Helper()
	reg := cmd.NewRegistry()
	cmd.RegisterBuiltins(reg, cmd.Deps{})
	sender := &recordingSender{}
	exec := &echoExecutor{}

	p, err := server.New(server.Config{
		Sender:      sender,
		Executor:    exec,
		Gate:        allowAllGate(),
		Route:       channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Commands:    reg,
		Permissions: perms,
		OwnerCheck:  ownerCheck,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return p, sender, exec
}

// lastText 返回最后一次出站的文本。
func lastText(s *recordingSender) string {
	calls := s.snapshot()
	if len(calls) == 0 {
		return ""
	}
	return calls[len(calls)-1].Text
}

// ── AC-C1/C2：/help 可用且不调用模型 ──

func TestCmd_HelpWorksAndDoesNotCallModel(t *testing.T) {
	perms := &fakePerms{allowed: map[string]bool{"cmd:help": true}}
	p, sender, exec := newCmdPipeline(t, perms, nil)

	if err := p.Handle(context.Background(), directMsg("om_c1", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// AC-C2 核心：不调用模型
	if n := len(exec.inputs()); n != 0 {
		t.Errorf("命令不应调用模型，实际 %d 次", n)
	}
	// AC-C1：输出含全部命令
	reply := lastText(sender)
	for _, n := range []string{"/help", "/status", "/clear", "/stop"} {
		if !strings.Contains(reply, n) {
			t.Errorf("/help 回复应含 %s，实际: %q", n, reply)
		}
	}
	t.Log("✓ /help 可用且未调用模型（0 次）")
}

// ── AC-C4：未注册的 /xxx 走正常消息路径 ──

func TestCmd_UnknownCommandGoesToModel(t *testing.T) {
	perms := &fakePerms{allowed: map[string]bool{"cmd:help": true}}
	p, _, exec := newCmdPipeline(t, perms, nil)

	// 用户可能发的路径文本——不应被误判为命令
	if err := p.Handle(context.Background(), directMsg("om_c2", "/usr/local/bin 是什么")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if n := len(exec.inputs()); n != 1 {
		t.Errorf("未注册的 /xxx 应走消息路径（调用模型 1 次），实际 %d 次", n)
	}
	t.Log("✓ /usr/local/bin 是什么 → 走消息路径（未误判为命令）")
}

// ── AC-C5：无权限返回明确拒绝 ──

func TestCmd_DeniedWithoutPermission(t *testing.T) {
	// 只有 help 权限，没有 clear
	perms := &fakePerms{allowed: map[string]bool{"cmd:help": true}}
	p, sender, exec := newCmdPipeline(t, perms, func(string) bool { return true })

	if err := p.Handle(context.Background(), directMsg("om_c3", "/clear")); err != nil {
		t.Fatalf("Handle 不应返回 error（拒绝是正常路径）: %v", err)
	}

	if n := len(exec.inputs()); n != 0 {
		t.Errorf("被拒命令不应调用模型，实际 %d 次", n)
	}
	reply := lastText(sender)
	if !strings.Contains(reply, "权限") {
		t.Errorf("拒绝回复应说明权限，实际: %q", reply)
	}
	// 复用 denyMessage 的措辞策略（确定性拒绝 + 重试无效）
	if !strings.Contains(reply, "重试") {
		t.Errorf("拒绝回复应告知重试无效（实测模型会连试 3 次），实际: %q", reply)
	}
	t.Logf("✓ 无权限命令被拒: %q", reply)
}

// ── OwnerOnly 判定（SPEC §7.1 ③）──

func TestCmd_OwnerOnlyDeniedForNonOwner(t *testing.T) {
	perms := &fakePerms{allowed: map[string]bool{"cmd:stop": true}}
	p, sender, _ := newCmdPipeline(t, perms, func(string) bool { return false })

	if err := p.Handle(context.Background(), directMsg("om_c4", "/stop")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply := lastText(sender); !strings.Contains(reply, "owner") {
		t.Errorf("非 owner 执行 OwnerOnly 命令应被拒，实际: %q", reply)
	}
	t.Logf("✓ OwnerOnly 拒绝: %q", lastText(sender))
}

// fail-closed：未装配 OwnerCheck 时 OwnerOnly 命令一律拒绝。
func TestCmd_OwnerOnlyFailsClosedWithoutOwnerCheck(t *testing.T) {
	perms := &fakePerms{allowed: map[string]bool{"cmd:stop": true}}
	p, sender, _ := newCmdPipeline(t, perms, nil)

	if err := p.Handle(context.Background(), directMsg("om_c5", "/stop")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply := lastText(sender); !strings.Contains(reply, "owner") {
		t.Errorf("未装配 OwnerCheck 应 fail-closed，实际: %q", reply)
	}
	t.Log("✓ 未装配 OwnerCheck 时 fail-closed")
}

// ── fail-closed：未配权限源时拒绝一切命令 ──

func TestCmd_FailsClosedWithoutPermissionSource(t *testing.T) {
	p, sender, exec := newCmdPipeline(t, nil, func(string) bool { return true })

	if err := p.Handle(context.Background(), directMsg("om_c6", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n := len(exec.inputs()); n != 0 {
		t.Error("未配权限源时不应调用模型")
	}
	if reply := lastText(sender); !strings.Contains(reply, "权限") {
		t.Errorf("未配权限源应拒绝，实际: %q", reply)
	}
	t.Log("✓ 未配权限源时 fail-closed（拒绝一切命令）")
}

// 权限源报错（查不了）→ 拒绝但文案区分（SPEC §6.1）。
func TestCmd_PermissionSourceErrorIsDistinguished(t *testing.T) {
	perms := &fakePerms{err: errors.New("backend down")}
	p, sender, _ := newCmdPipeline(t, perms, func(string) bool { return true })

	if err := p.Handle(context.Background(), directMsg("om_c7", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply := lastText(sender); !strings.Contains(reply, "暂时") {
		t.Errorf("「查不了」的文案应与「不允许」区分，实际: %q", reply)
	}
	t.Logf("✓ 权限源故障的文案区分: %q", lastText(sender))
}

// ── AC-C7：私聊与群聊均可达 ──

func TestCmd_WorksInBothChatTypes(t *testing.T) {
	perms := &fakePerms{allowed: map[string]bool{"cmd:help": true}}
	p, sender, _ := newCmdPipeline(t, perms, nil)

	// 私聊
	if err := p.Handle(context.Background(), directMsg("om_dm", "/help")); err != nil {
		t.Fatalf("私聊 Handle: %v", err)
	}
	if n := len(sender.snapshot()); n != 1 {
		t.Errorf("私聊应收到 1 条回复，实际 %d", n)
	}

	// 群聊：需 @bot 才过门禁。allowAllGate 的 Activation 决定是否需要 mention。
	// 用 groupMsg 并传 mention=true 保证过门禁。
	gm := groupMsg("om_group", "/help", "", true)
	if err := p.Handle(context.Background(), gm); err != nil {
		t.Fatalf("群聊 Handle: %v", err)
	}
	if n := len(sender.snapshot()); n != 2 {
		t.Errorf("群聊应再收到 1 条回复，实际总计 %d", n)
	}
	t.Log("✓ 私聊与群聊均可执行命令")
}

// ── 向后兼容：Commands 为 nil 时 /help 走消息路径 ──

func TestCmd_NilRegistryKeepsLegacyBehaviour(t *testing.T) {
	sender := &recordingSender{}
	exec := &echoExecutor{}
	p, err := server.New(server.Config{
		Sender:   sender,
		Executor: exec,
		Gate:     allowAllGate(),
		Route:    channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		// Commands 留空 = 禁用命令功能
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	if err := p.Handle(context.Background(), directMsg("om_c8", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// 命令功能禁用时，/help 是普通文本 → 送模型
	if n := len(exec.inputs()); n != 1 {
		t.Errorf("Commands 为 nil 时 /help 应走消息路径，实际模型调用 %d 次", n)
	}
	t.Log("✓ Commands 为 nil 时行为与既有一致（向后兼容）")
}

// ── 命令绕过门禁的防护（SPEC §5.5）──

func TestCmd_DoesNotBypassGate(t *testing.T) {
	perms := &fakePerms{allowed: map[string]bool{"cmd:help": true}}
	reg := cmd.NewRegistry()
	cmd.RegisterBuiltins(reg, cmd.Deps{})
	sender := &recordingSender{}
	exec := &echoExecutor{}

	// 门禁配为 disabled（硬停止）
	p, err := server.New(server.Config{
		Sender:      sender,
		Executor:    exec,
		Gate:        server.GateConfig{Activation: channel.ActivationDisabled},
		Route:       channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Commands:    reg,
		Permissions: perms,
		OwnerCheck:  func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	if err := p.Handle(context.Background(), directMsg("om_c9", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 门禁拒绝 → 静默丢弃，命令也不执行
	if n := len(sender.snapshot()); n != 0 {
		t.Errorf("门禁拒绝时不应有出站（含命令回复），实际 %d 条", n)
	}
	if n := len(exec.inputs()); n != 0 {
		t.Error("门禁拒绝时不应调用模型")
	}
	t.Log("✓ 命令不绕过门禁（disabled 时静默丢弃）")
}

// ── AC-D3：命令权限经 RBAC 生效（真 RBACPermissions）──

func TestCmd_PermissionViaRealRBAC(t *testing.T) {
	rbac := authz.NewRBACPermissions(authz.RBACConfig{
		Roles: map[string][]string{
			"viewer":   {"cmd:help", "cmd:status"},
			"operator": {"cmd:clear"},
		},
		RoleParents: map[string][]string{"operator": {"viewer"}},
		UserRoles:   map[string][]string{"ws1:feishu:ou_sender": {"operator"}},
	})
	p, sender, _ := newCmdPipeline(t, rbac, func(string) bool { return true })

	// directMsg（e2e_test.go 夹具）的 UserID 是 "ou_sender"，Principal 形如 ws1:feishu:ou_sender
	// 它是 operator，继承 viewer → 可用 /help
	if err := p.Handle(context.Background(), directMsg("om_r1", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply := lastText(sender); !strings.Contains(reply, "可用命令") {
		t.Errorf("operator（继承 viewer）应能用 /help，实际: %q", reply)
	}

	// /stop 未授予 → 拒绝
	if err := p.Handle(context.Background(), directMsg("om_r2", "/stop")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply := lastText(sender); !strings.Contains(reply, "权限") {
		t.Errorf("未授予的 /stop 应被拒，实际: %q", reply)
	}
	t.Log("✓ 真 RBAC 生效：继承的 /help 通过，未授予的 /stop 被拒")
}

// ── 命令不写会话历史（AC-C3）──

func TestCmd_DoesNotEnterSessionHistory(t *testing.T) {
	perms := &fakePerms{allowed: map[string]bool{"cmd:help": true}}
	p, _, exec := newCmdPipeline(t, perms, nil)

	if err := p.Handle(context.Background(), directMsg("om_h1", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 命令不进执行层 → executor 收到的输入里没有命令文本
	for _, in := range exec.inputs() {
		if strings.HasPrefix(in, "/") {
			t.Errorf("命令文本不应进入执行层（会污染会话历史），实际: %q", in)
		}
	}
	t.Log("✓ 命令未进入执行层（不写会话历史）")
}
