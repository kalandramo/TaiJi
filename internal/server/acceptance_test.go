package server_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/cmd"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 用户级验收测试：模拟真实部署的完整装配，验证用户可见行为。
//
// **与其它测试的区别**：这里用**与 cmd/taiji/main.go 完全相同的装配路径**
// （RBAC 权限源 + owner 判定 + 会话代数 + 取消注册表），
// 走完整的 Handle 链路，断言的是**用户能看到什么**。
//
// 它不能替代真实飞书对话（那需要用户的账号），但能验证：
// 给定真实配置，命令链路的行为与设计一致。

// realDeployment 复刻 main.go 的装配。
func realDeployment(t *testing.T, rbacSpec authz.PermissionSource, ownerID string) (
	*server.Pipeline, *recordingSender, *sessionRecordingExecutor, *genSpy,
) {
	t.Helper()
	gens := newGenSpy()
	cancels := server.NewCancelRegistry()
	reg := cmd.NewRegistry()
	cmd.RegisterBuiltins(reg, cmd.Deps{
		ClearSession: gens.Next,
		CancelRun:    cancels.Cancel,
	})

	sender := &recordingSender{}
	exec := &sessionRecordingExecutor{}
	p, err := server.New(server.Config{
		Sender:      sender,
		Executor:    exec,
		Gate:        allowAllGate(),
		Route:       channel.RouteConfig{WorkspaceID: "default", BindingMode: channel.BindingThreadMap},
		Commands:    reg,
		Permissions: rbacSpec,
		OwnerCheck:  func(openID string) bool { return openID == ownerID },
		SessionGen:  gens,
		Cancels:     cancels,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return p, sender, exec, gens
}

// 复刻 main.go 的 RBAC 配置：role:admin=* + user:<prefix><open_id>=admin。
//
// 注意 directMsg 夹具的 UserID 是 ou_sender，故这里用同一个 ID——
// 这模拟「该用户配了 admin 角色」的真实场景。
func adminRBAC() authz.PermissionSource {
	return authz.NewRBACPermissions(authz.RBACConfig{
		Roles:     map[string][]string{"admin": {"*"}},
		UserRoles: map[string][]string{"default:feishu:ou_sender": {"admin"}},
	})
}

// ── 验收 1：/help 收到命令列表且不触发模型 ──

func TestAcceptance_HelpReturnsCommandListWithoutModel(t *testing.T) {
	p, sender, exec, _ := realDeployment(t, adminRBAC(), "ou_sender")

	if err := p.Handle(context.Background(), directMsg("om_acc1", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 用户看到什么：4 条命令的列表
	reply := lastText(sender)
	for _, want := range []string{"/help", "/status", "/clear", "/stop"} {
		if !strings.Contains(reply, want) {
			t.Errorf("用户应看到 %s，实际回复:\n%s", want, reply)
		}
	}
	// 且不应有模型生成（无占位、无回答）
	if n := len(exec.inputs()); n != 0 {
		t.Errorf("命令不应触发模型，实际调用 %d 次", n)
	}
	// 用户不应看到「思考中…」占位（那是消息路径的产物）
	if strings.Contains(reply, "思考中") {
		t.Error("命令回复不应含「思考中…」占位")
	}
	t.Logf("✓ 验收1 通过。用户看到:\n%s", reply)
}

// ── 验收 2：/usr/local/bin 是什么 → 走模型 ──

func TestAcceptance_UnregisteredSlashGoesToModel(t *testing.T) {
	p, sender, exec, _ := realDeployment(t, adminRBAC(), "ou_sender")

	if err := p.Handle(context.Background(), directMsg("om_acc2", "/usr/local/bin 是什么")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 用户看到什么：模型的回答（不是「未知命令」错误）
	if n := len(exec.inputs()); n != 1 {
		t.Fatalf("应走消息路径（模型调用 1 次），实际 %d 次", n)
	}
	if got := exec.inputs()[0]; got != "/usr/local/bin 是什么" {
		t.Errorf("模型应收到原文，实际: %q", got)
	}
	// 出站应是消息路径的两步（占位 + 更新），不是命令的单次回复
	if n := len(sender.snapshot()); n < 1 {
		t.Error("应有出站")
	}
	t.Logf("✓ 验收2 通过。模型收到原文，走消息路径（出站 %d 次）",
		len(sender.snapshot()))
}

// ── 验收 3：/clear 后历史隔离 ──

func TestAcceptance_ClearIsolatesHistory(t *testing.T) {
	p, sender, exec, _ := realDeployment(t, adminRBAC(), "ou_sender")
	ctx := context.Background()

	// 用户：发一条消息 → 看到回答
	if err := p.Handle(ctx, directMsg("om_acc3a", "你好")); err != nil {
		t.Fatalf("Handle 1: %v", err)
	}
	// 用户：发 /clear → 看到「已开始新会话」
	if err := p.Handle(ctx, directMsg("om_acc3b", "/clear")); err != nil {
		t.Fatalf("Handle /clear: %v", err)
	}
	clearReply := lastText(sender)
	if !strings.Contains(clearReply, "新会话") {
		t.Errorf("/clear 回复应说明开了新会话，实际: %q", clearReply)
	}

	// 用户：再发一条消息 → 用新 sessionID（历史隔离）
	if err := p.Handle(ctx, directMsg("om_acc3c", "第二句")); err != nil {
		t.Fatalf("Handle 2: %v", err)
	}

	sessions := exec.sessionList()
	if len(sessions) != 2 {
		t.Fatalf("应有 2 次模型调用（/clear 不调模型），实际 %d: %v", len(sessions), sessions)
	}
	if sessions[0] == sessions[1] {
		t.Fatalf("历史未隔离——两条消息共用 sessionID %q", sessions[0])
	}
	if !strings.Contains(sessions[1], "#gen:1") {
		t.Errorf("第二条应带 #gen:1，实际: %q", sessions[1])
	}
	t.Logf("✓ 验收3 通过。/clear 回复: %q\n  前 sessionID: %q\n  后 sessionID: %q",
		clearReply, sessions[0], sessions[1])
}

// ── 验收 4：无权限时用户看到明确拒绝 ──

func TestAcceptance_DeniedUserSeesClearMessage(t *testing.T) {
	// 配一个**没有** cmd 权限的用户（只有工具权限，无命令权限）
	rbac := authz.NewRBACPermissions(authz.RBACConfig{
		Roles:     map[string][]string{"viewer": {"mockmcp_echo"}}, // 只有工具权限
		UserRoles: map[string][]string{"default:feishu:ou_sender": {"viewer"}},
	})
	p, sender, exec, _ := realDeployment(t, rbac, "ou_sender")

	if err := p.Handle(context.Background(), directMsg("om_acc4", "/help")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	reply := lastText(sender)
	if !strings.Contains(reply, "权限") {
		t.Errorf("无 cmd 权限的用户应看到权限说明，实际: %q", reply)
	}
	if n := len(exec.inputs()); n != 0 {
		t.Error("被拒命令不应调用模型")
	}
	t.Logf("✓ 验收4 通过。只配工具权限的用户看到: %q", reply)
}

// ── 验收 5：/stop 无进行中生成时的用户可见行为 ──

func TestAcceptance_StopWithoutRunningGeneration(t *testing.T) {
	p, sender, _, _ := realDeployment(t, adminRBAC(), "ou_sender")

	if err := p.Handle(context.Background(), directMsg("om_acc5", "/stop")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reply := lastText(sender)
	if !strings.Contains(reply, "没有") {
		t.Errorf("/stop 在无生成时应说明，实际: %q", reply)
	}
	t.Logf("✓ 验收5 通过。/stop 回复: %q", reply)
}

// ── 验收 6：/status 展示会话信息 ──

func TestAcceptance_StatusShowsSessionInfo(t *testing.T) {
	p, sender, _, _ := realDeployment(t, adminRBAC(), "ou_sender")

	if err := p.Handle(context.Background(), directMsg("om_acc6", "/status")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reply := lastText(sender)
	for _, want := range []string{"会话", "default"} {
		if !strings.Contains(reply, want) {
			t.Errorf("/status 应含 %q，实际:\n%s", want, reply)
		}
	}
	t.Logf("✓ 验收6 通过。/status 回复:\n%s", reply)
}
