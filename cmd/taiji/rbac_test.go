package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
)

// TAIJI_RBAC 环境变量解析（Wave 2——决策三）。
//
// 格式：role:<名>=<权限>; parent:<子>=<父>; user:<主体>=<角色>（分号分隔条目）。

func TestEnvRBAC_UnsetIsNil(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "")
	if got, err := envRBAC(); err != nil || got != nil {
		t.Errorf("未配置应返回 nil，got %v", got)
	}
}

func TestEnvRBAC_ParsesRolesAndUsers(t *testing.T) {
	t.Setenv("TAIJI_RBAC",
		"role:admin=*;role:operator=mockmcp_echo,infraverse_*;"+
			"user:ws1:feishu:ou_alice=admin;user:ws1:feishu:ou_bob=operator")

	src, err := envRBAC()
	if err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
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

	src, err := envRBAC()
	if err != nil {
		t.Fatalf("envRBAC: %v", err)
	}
	ok, _ := src.Allowed(context.Background(), authz.AccessRequest{
		Principal: authz.Principal{ID: "u1"}, Action: "read_x",
	})
	if !ok {
		t.Error("应继承父角色 viewer 的 read_* 权限")
	}
}

// resolvePermissions 的唯一来源是 RBAC。
//
// 2026-09-26 更新：TAIJI_USER_PERMISSIONS 退役后不再有回退路径。
// 原 TestResolvePermissions_FallsBackToStatic 已删除——它断言的
// 「未配 RBAC 时回退静态表」行为**正是被退役的**。

// 两者都配时，仍用 RBAC（退役变量被忽略）。
func TestResolvePermissions_IgnoresRetiredStatic(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "role:admin=*;user:u=admin")
	t.Setenv("TAIJI_USER_PERMISSIONS", "ws1:feishu:ou_alice=mockmcp_echo")

	src, err := resolvePermissions()
	if err != nil {
		t.Fatalf("resolvePermissions: %v", err)
	}
	if _, ok := src.(*authz.RBACPermissions); !ok {
		t.Errorf("应返回 RBAC，got %T", src)
	}
}

// 只配退役变量时无权限源（其值不再生效）。
func TestResolvePermissions_RetiredStaticAloneIsNil(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "")
	t.Setenv("TAIJI_USER_PERMISSIONS", "ws1:feishu:ou_alice=mockmcp_echo")

	src, err := resolvePermissions()
	if err != nil {
		t.Fatalf("resolvePermissions: %v", err)
	}
	if src != nil {
		t.Errorf("退役变量不应产生权限源，got %T", src)
	}
}

func TestResolvePermissions_NeitherIsNil(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "")
	t.Setenv("TAIJI_USER_PERMISSIONS", "")
	if got, err := resolvePermissions(); err != nil || got != nil {
		t.Errorf("两者都未配应返回 nil，got %v", got)
	}
}

// envRBAC 对配置错误 fail-fast（而非静默让用户被拒）。
//
// 实测动机：`user:u=admn`（角色名拼错）原会被静默接受，该用户权限为空
// → 所有工具调用被拒且无提示。现在应在解析期报错。
func TestEnvRBAC_UnknownRoleFailsFast(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "role:admin=*;user:u=admn") // admn 未定义
	_, err := envRBAC()
	if err == nil {
		t.Fatal("引用未定义角色应报错（否则该用户被静默拒绝）")
	}
	if !strings.Contains(err.Error(), "admn") {
		t.Errorf("错误信息应指出未定义的角色名，got: %v", err)
	}
}

// 合法配置不应报错（回归护栏）。
func TestEnvRBAC_ValidConfigNoError(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "role:admin=*;role:viewer=read_*;parent:admin=viewer;user:u=admin")
	if _, err := envRBAC(); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}
}

// resolvePermissions 在 RBAC 配置错误时应向上传递 error（供 serve fail-fast）。
func TestResolvePermissions_PropagatesRBACError(t *testing.T) {
	t.Setenv("TAIJI_RBAC", "role:admin=*;user:u=nope")
	_, err := resolvePermissions()
	if err == nil {
		t.Fatal("RBAC 配置错误应向上传递，而非回退到静态表")
	}
}

// 文档示例的配置形态必须能被解析并正确匹配（防文档-实现漂移）。
//
// 背景（实测发现的漂移）：文档 §4.4 示例曾写 `role:operator=tool:mockmcp_echo`
// （带 tool: 前缀），但实现匹配**裸工具名**——照示例配会静默拒绝。
// 本测试把**当前文档示例**固化为可执行断言：文档与实现不一致时变红。
//
// 当前约定：v1 权限点用**裸工具名**（MCP 工具名本身已带 {server}_ 前缀，
// 再叠 tool: 是冗余）；v2 资源级动作用 "ws:" 等前缀区分（非工具名）。
func TestEnvRBAC_DocExampleConfigWorks(t *testing.T) {
	// 与文档 §4.4 示例一致（裸工具名形态）。
	t.Setenv("TAIJI_RBAC",
		"role:admin=*;role:operator=mockmcp_echo,infraverse_*;"+
			"user:ws1:feishu:ou_alice=admin;user:ws1:feishu:ou_bob=operator")

	src, err := envRBAC()
	if err != nil {
		t.Fatalf("文档示例应能被解析: %v", err)
	}
	ctx := context.Background()

	// admin（*）→ 任意工具
	if ok, _ := src.Allowed(ctx, authz.AccessRequest{
		Principal: authz.Principal{ID: "ws1:feishu:ou_alice"}, Action: "mockmcp_echo",
	}); !ok {
		t.Error("admin 应放行 mockmcp_echo（文档示例）")
	}
	// operator → 精确名
	if ok, _ := src.Allowed(ctx, authz.AccessRequest{
		Principal: authz.Principal{ID: "ws1:feishu:ou_bob"}, Action: "mockmcp_echo",
	}); !ok {
		t.Error("operator 应放行 mockmcp_echo（文档示例的精确名）")
	}
	// operator → 通配
	if ok, _ := src.Allowed(ctx, authz.AccessRequest{
		Principal: authz.Principal{ID: "ws1:feishu:ou_bob"}, Action: "infraverse_dce_ip",
	}); !ok {
		t.Error("operator 的 infraverse_* 应匹配（文档示例的通配）")
	}
}
