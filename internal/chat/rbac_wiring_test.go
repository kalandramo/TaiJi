package chat

import (
	"context"
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
)

// RBAC 的端到端接线验证（遗留 ①）。
//
// 为什么需要它（缺口 1 的教训）：既有 permission_wiring_test.go 用 **fake**
// 权限源（recordingPermissionSource）——它证明了「插件被挂载」，但**没证明
// 「真 RBAC 被插件消费」**。单测证明 RBAC 逻辑对、插件测试证明挂载对，
// 两者的**接缝**（envRBAC 产出的 RBAC 真的被插件判定）此前无覆盖。
//
// 本文件用**真 RBACPermissions** 走完整链路：Executor → 框架 → beforeTool
// → RBACPermissions.Allowed。
//
// 注：envRBAC 的解析（TAIJI_RBAC 字符串 → RBACPermissions）在 cmd/taiji
// 的测试里覆盖；这里聚焦「RBAC 实例被消费」这一接缝。

// rbacPermExecutor 装配一个用真 RBAC 的执行器。
func rbacPermExecutor(t *testing.T, srvURL string, cfg authz.RBACConfig) *Executor {
	t.Helper()
	return permTestExecutor(t, srvURL, authz.NewRBACPermissions(cfg))
}

// 经 RBAC 授权的用户 → 工具真的执行。
func TestRBACWiring_AuthorizedUserCanUseTool(t *testing.T) {
	srv := toolCallSSE(t)
	ex := rbacPermExecutor(t, srv.URL, authz.RBACConfig{
		Roles:     map[string][]string{"operator": {"mockmcp_echo"}},
		UserRoles: map[string][]string{"ws1:feishu:ou_alice": {"operator"}},
	})

	ctx := authz.WithPrincipal(context.Background(),
		authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_alice"})

	answer, err := ex.Execute(ctx, "rbac-allow", "用 echo 说你好")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(answer, "Echo:") {
		t.Errorf("经 RBAC 授权的用户应能执行工具，回答 = %q", answer)
	}
	t.Logf("✓ RBAC 授权用户执行成功，回答 = %q", answer)
}

// 未绑定角色的用户 → 工具不执行（fail-closed）。
func TestRBACWiring_UnboundUserBlocked(t *testing.T) {
	srv := toolCallSSE(t)
	ex := rbacPermExecutor(t, srv.URL, authz.RBACConfig{
		Roles:     map[string][]string{"operator": {"mockmcp_echo"}},
		UserRoles: map[string][]string{"ws1:feishu:ou_alice": {"operator"}},
	})

	// bob 未绑定任何角色
	ctx := authz.WithPrincipal(context.Background(),
		authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_bob"})

	answer, err := ex.Execute(ctx, "rbac-deny", "用 echo 说你好")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if contains(answer, "Echo:") {
		t.Errorf("未绑定角色的用户不应执行工具，回答 = %q", answer)
	}
	t.Logf("✓ RBAC 未绑定用户被拦，回答 = %q", answer)
}

// 角色未授权该工具 → 不执行（授权了别的工具也不放行）。
func TestRBACWiring_RoleNotGrantingToolBlocked(t *testing.T) {
	srv := toolCallSSE(t)
	ex := rbacPermExecutor(t, srv.URL, authz.RBACConfig{
		Roles:     map[string][]string{"viewer": {"mockmcp_other"}}, // 不含 echo
		UserRoles: map[string][]string{"ws1:feishu:ou_alice": {"viewer"}},
	})

	ctx := authz.WithPrincipal(context.Background(),
		authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_alice"})

	answer, err := ex.Execute(ctx, "rbac-wrongtool", "用 echo 说你好")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if contains(answer, "Echo:") {
		t.Errorf("角色未授权该工具时不应执行，回答 = %q", answer)
	}
	t.Logf("✓ RBAC 未授权工具被拦，回答 = %q", answer)
}

// 继承的权限也能经完整链路生效（RBAC1 端到端）。
func TestRBACWiring_InheritedPermissionWorks(t *testing.T) {
	srv := toolCallSSE(t)
	ex := rbacPermExecutor(t, srv.URL, authz.RBACConfig{
		Roles: map[string][]string{
			"viewer":   {"mockmcp_echo"},
			"operator": {}, // 无直接权限
		},
		RoleParents: map[string][]string{"operator": {"viewer"}},
		UserRoles:   map[string][]string{"ws1:feishu:ou_alice": {"operator"}},
	})

	ctx := authz.WithPrincipal(context.Background(),
		authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_alice"})

	answer, err := ex.Execute(ctx, "rbac-inherit", "用 echo 说你好")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(answer, "Echo:") {
		t.Errorf("继承的权限应经完整链路生效，回答 = %q", answer)
	}
	t.Logf("✓ RBAC 继承权限生效，回答 = %q", answer)
}
