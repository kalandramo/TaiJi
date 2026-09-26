package authz

import (
	"context"
	"testing"
)

// RBAC 权限源的单测（Wave 2——决策三）。
//
// 模型：User → Role → Permission，含 RBAC1 角色继承。
//
//	User ──N×R──▶ Role ──R×M──▶ Permission（裸工具名 / 通配）
//
// 与 StaticPermissions 的关系：后者是 User 直连 Permission（RBAC0 退化），
// 本实现补上 Role 层——用户规模上来后配置量从 N×M 降到 N+R×M。

// rbac 是测试用的构造助手：roles 与 userRoles 直接给出。
func newRBAC(roles map[string][]string, userRoles map[string][]string) *RBACPermissions {
	return NewRBACPermissions(RBACConfig{Roles: roles, UserRoles: userRoles})
}

func TestRBAC_DirectRoleGrantsTool(t *testing.T) {
	p := newRBAC(
		map[string][]string{"operator": {"mockmcp_echo"}},
		map[string][]string{"ws1:feishu:ou_alice": {"operator"}},
	)
	ok, err := p.Allowed(context.Background(), AccessRequest{
		Principal: Principal{ID: "ws1:feishu:ou_alice"},
		Action:    "mockmcp_echo",
	})
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if !ok {
		t.Error("经角色授权的工具应放行")
	}
}

func TestRBAC_RoleNotGrantingToolDenies(t *testing.T) {
	p := newRBAC(
		map[string][]string{"operator": {"mockmcp_echo"}},
		map[string][]string{"ws1:feishu:ou_alice": {"operator"}},
	)
	ok, _ := p.Allowed(context.Background(), AccessRequest{
		Principal: Principal{ID: "ws1:feishu:ou_alice"},
		Action:    "srv_dangerous",
	})
	if ok {
		t.Error("角色未授权的工具应被拒")
	}
}

// 未绑定任何角色的用户 → 拒绝（fail-closed）。
func TestRBAC_UnboundUserDenied(t *testing.T) {
	p := newRBAC(
		map[string][]string{"operator": {"*"}},
		map[string][]string{"ws1:feishu:ou_alice": {"operator"}},
	)
	ok, err := p.Allowed(context.Background(), AccessRequest{
		Principal: Principal{ID: "ws1:feishu:ou_mallory"},
		Action:    "mockmcp_echo",
	})
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if ok {
		t.Error("未绑定角色的用户应被拒（fail-closed）")
	}
}

// 空主体 → 拒绝。
func TestRBAC_EmptyPrincipalDenied(t *testing.T) {
	p := newRBAC(map[string][]string{"admin": {"*"}}, nil)
	ok, _ := p.Allowed(context.Background(), AccessRequest{Action: "x"})
	if ok {
		t.Error("空主体应被拒")
	}
}

// nil/空配置 → 拒绝一切（安全基线）。
func TestRBAC_EmptyConfigDeniesAll(t *testing.T) {
	p := NewRBACPermissions(RBACConfig{})
	ok, _ := p.Allowed(context.Background(), AccessRequest{
		Principal: Principal{ID: "ws1:feishu:ou_alice"},
		Action:    "mockmcp_echo",
	})
	if ok {
		t.Error("空配置应拒绝一切")
	}
}

// 角色继承（RBAC1）：子角色继承父角色的权限。
func TestRBAC_RoleInheritance(t *testing.T) {
	p := NewRBACPermissions(RBACConfig{
		// operator 继承 viewer 的权限，并额外有 write 权限。
		Roles: map[string][]string{
			"viewer":   {"tool:read_*"},
			"operator": {"tool:write_*"},
		},
		RoleParents: map[string][]string{
			"operator": {"viewer"}, // operator 的父角色是 viewer
		},
		UserRoles: map[string][]string{"ws1:feishu:ou_alice": {"operator"}},
	})
	ctx := context.Background()
	alice := Principal{ID: "ws1:feishu:ou_alice"}

	// 直接权限
	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: alice, Action: "tool:write_x"}); !ok {
		t.Error("operator 的直接权限应生效")
	}
	// 继承权限
	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: alice, Action: "tool:read_x"}); !ok {
		t.Error("应从父角色 viewer 继承 read 权限")
	}
	// 未授权
	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: alice, Action: "tool:delete_x"}); ok {
		t.Error("未授权动作应被拒")
	}
}

// 多级继承：A → B → C，A 应继承 C 的权限。
func TestRBAC_MultiLevelInheritance(t *testing.T) {
	p := NewRBACPermissions(RBACConfig{
		Roles: map[string][]string{
			"c": {"tool:base"},
			"b": {},
			"a": {"tool:top"},
		},
		RoleParents: map[string][]string{
			"a": {"b"},
			"b": {"c"},
		},
		UserRoles: map[string][]string{"u": {"a"}},
	})
	ok, _ := p.Allowed(context.Background(), AccessRequest{
		Principal: Principal{ID: "u"}, Action: "tool:base",
	})
	if !ok {
		t.Error("多级继承应传递到最底层角色")
	}
}

// 继承环（配置错误）不应死循环——必须终止。
func TestRBAC_InheritanceCycleTerminates(t *testing.T) {
	p := NewRBACPermissions(RBACConfig{
		Roles: map[string][]string{
			"a": {"tool:x"},
			"b": {},
		},
		RoleParents: map[string][]string{
			"a": {"b"},
			"b": {"a"}, // 环
		},
		UserRoles: map[string][]string{"u": {"a"}},
	})
	// 若实现有环处理，此调用会返回；否则测试超时（也算失败信号）。
	ok, _ := p.Allowed(context.Background(), AccessRequest{
		Principal: Principal{ID: "u"}, Action: "tool:x",
	})
	if !ok {
		t.Error("环中的直接权限仍应生效")
	}
}

// 通配：角色权限支持 "*" 与前缀通配（复用 matchToolPattern 语义）。
func TestRBAC_WildcardPermissions(t *testing.T) {
	p := newRBAC(
		map[string][]string{
			"admin":    {"*"},
			"infra":    {"infraverse_*"},
			"explicit": {"mockmcp_echo"},
		},
		map[string][]string{
			"u_admin":    {"admin"},
			"u_infra":    {"infra"},
			"u_explicit": {"explicit"},
		},
	)
	ctx := context.Background()

	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: Principal{ID: "u_admin"}, Action: "anything"}); !ok {
		t.Error("\"*\" 应放行任意工具")
	}
	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: Principal{ID: "u_infra"}, Action: "infraverse_dce_ip"}); !ok {
		t.Error("infraverse_* 应匹配 infraverse_dce_ip")
	}
	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: Principal{ID: "u_infra"}, Action: "mockmcp_echo"}); ok {
		t.Error("infraverse_* 不应匹配 mockmcp_echo")
	}
	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: Principal{ID: "u_explicit"}, Action: "mockmcp_echo"}); !ok {
		t.Error("精确匹配应放行")
	}
}

// 空动作 → 拒绝（无动作可判定）。
func TestRBAC_EmptyActionDenied(t *testing.T) {
	p := newRBAC(map[string][]string{"admin": {"*"}}, map[string][]string{"u": {"admin"}})
	ok, _ := p.Allowed(context.Background(), AccessRequest{Principal: Principal{ID: "u"}})
	if ok {
		t.Error("空动作应被拒")
	}
}

// 一个用户多个角色 → 权限并集。
func TestRBAC_MultipleRolesUnion(t *testing.T) {
	p := newRBAC(
		map[string][]string{
			"reader": {"tool:read"},
			"writer": {"tool:write"},
		},
		map[string][]string{"u": {"reader", "writer"}},
	)
	ctx := context.Background()
	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: Principal{ID: "u"}, Action: "tool:read"}); !ok {
		t.Error("多角色的第一个角色权限应生效")
	}
	if ok, _ := p.Allowed(ctx, AccessRequest{Principal: Principal{ID: "u"}, Action: "tool:write"}); !ok {
		t.Error("多角色的第二个角色权限应生效")
	}
}

// UserCount / RoleCount 供启动期日志。
func TestRBAC_Counts(t *testing.T) {
	p := NewRBACPermissions(RBACConfig{
		Roles:     map[string][]string{"a": {"x"}, "b": {"y"}},
		UserRoles: map[string][]string{"u1": {"a"}, "u2": {"b"}},
	})
	if got := p.RoleCount(); got != 2 {
		t.Errorf("RoleCount = %d, want 2", got)
	}
	if got := p.UserCount(); got != 2 {
		t.Errorf("UserCount = %d, want 2", got)
	}
}
