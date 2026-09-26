package main

import (
	"context"
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
)

// TAIJI_RBAC 环境变量解析（Wave 2——决策三）。
//
// 格式：role:<名>=<权限>; parent:<子>=<父>; user:<主体>=<角色>（分号分隔条目）。

func TestEnvRBAC_UnsetIsNil(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "")
	if got := envRBAC(); got != nil {
		t.Errorf("未配置应返回 nil，got %v", got)
	}
}

func TestEnvRBAC_ParsesRolesAndUsers(t *testing.T) {
	t.Setenv("TAIJI_RBAC",
		"role:admin=*;role:operator=mockmcp_echo,infraverse_*;"+
			"user:ws1:feishu:ou_alice=admin;user:ws1:feishu:ou_bob=operator")

	src := envRBAC()
	if src == nil {
		t.Fatal("合法配置应被解析")
	}
	rbac, ok := src.(*authz.RBACPermissions)
	if !ok {
		t.Fatalf("应返回 *RBACPermissions，got %T", src)
	}
	if rbac.RoleCount() != 2 {
		t.Errorf("RoleCount = %d, want 2", rbac.RoleCount())
	}
	if rbac.UserCount() != 2 {
		t.Errorf("UserCount = %d, want 2", rbac.UserCount())
	}

	ctx := context.Background()
	// alice 是 admin（*）→ 任意工具
	if ok, _ := rbac.Allowed(ctx, authz.AccessRequest{
		Principal: authz.Principal{ID: "ws1:feishu:ou_alice"}, Action: "anything",
	}); !ok {
		t.Error("admin 应放行任意工具")
	}
	// bob 是 operator → 通配
	if ok, _ := rbac.Allowed(ctx, authz.AccessRequest{
		Principal: authz.Principal{ID: "ws1:feishu:ou_bob"}, Action: "infraverse_dce_ip",
	}); !ok {
		t.Error("operator 的通配应生效")
	}
	// bob 不能用未授权工具
	if ok, _ := rbac.Allowed(ctx, authz.AccessRequest{
		Principal: authz.Principal{ID: "ws1:feishu:ou_bob"}, Action: "dangerous",
	}); ok {
		t.Error("未授权工具应被拒")
	}
}

func TestEnvRBAC_ParsesInheritance(t *testing.T) {
	t.Setenv("TAIJI_RBAC",
		"role:viewer=read_*;role:operator=write_*;"+
			"parent:operator=viewer;"+
			"user:u1=operator")

	src := envRBAC()
	ok, _ := src.Allowed(context.Background(), authz.AccessRequest{
		Principal: authz.Principal{ID: "u1"}, Action: "read_x",
	})
	if !ok {
		t.Error("应继承父角色 viewer 的 read_* 权限")
	}
}

// resolvePermissions 优先级：RBAC > 静态表。
func TestResolvePermissions_RBACTakesPrecedence(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "role:admin=*;user:u=admin")
	t.Setenv("TAIJI_USER_PERMISSIONS", "ws1:feishu:ou_alice=mockmcp_echo")

	src := resolvePermissions()
	if _, ok := src.(*authz.RBACPermissions); !ok {
		t.Errorf("两者都配时应优先 RBAC，got %T", src)
	}
}

func TestResolvePermissions_FallsBackToStatic(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "")
	t.Setenv("TAIJI_USER_PERMISSIONS", "ws1:feishu:ou_alice=mockmcp_echo")

	src := resolvePermissions()
	if _, ok := src.(*authz.StaticPermissions); !ok {
		t.Errorf("未配 RBAC 时应回退静态表，got %T", src)
	}
}

func TestResolvePermissions_NeitherIsNil(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "")
	t.Setenv("TAIJI_USER_PERMISSIONS", "")
	if got := resolvePermissions(); got != nil {
		t.Errorf("两者都未配应返回 nil，got %v", got)
	}
}
