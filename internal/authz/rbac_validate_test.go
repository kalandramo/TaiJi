package authz

import (
	"strings"
	"testing"
)

// ── RBAC 配置校验（2026-09-26 补）──
//
// 动机（实测发现的静默失效）：envRBAC 原实现对拼写错误静默跳过——
// `user:u=admn`（角色名拼错）会让用户绑定到一个**不存在的角色**，
// 权限为空 → 该用户所有工具调用被拒，而启动无任何提示。
// 这是「配了却不生效」的静默失效，最难排查。
//
// 校验规则：所有被引用的角色名必须**已定义**——定义来源有三：
//   - role: 的键（有直接权限的角色）
//   - parent: 的键（分组角色，可能无直接权限）
//   - parent: 的值（被继承的父角色）

func TestRBACConfig_Validate_UserReferencesUnknownRole(t *testing.T) {
	cfg := RBACConfig{
		Roles:     map[string][]string{"admin": {"*"}},
		UserRoles: map[string][]string{"u": {"admn"}}, // 拼写错误
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("user 引用未定义角色应报错（否则静默拒绝该用户）")
	}
}

func TestRBACConfig_Validate_ParentReferencesUnknownRole(t *testing.T) {
	cfg := RBACConfig{
		Roles:       map[string][]string{"a": {"x"}},
		RoleParents: map[string][]string{"a": {"nonexistent"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("parent 引用未定义角色应报错（死引用）")
	}
}

func TestRBACConfig_Validate_ValidConfigOK(t *testing.T) {
	cfg := RBACConfig{
		Roles:       map[string][]string{"admin": {"*"}, "viewer": {"read_*"}},
		UserRoles:   map[string][]string{"u": {"admin"}},
		RoleParents: map[string][]string{"admin": {"viewer"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}
}

// 角色仅作为 parent 的键出现（分组角色，无直接权限）——合法。
func TestRBACConfig_Validate_RoleDefinedOnlyAsParentKey(t *testing.T) {
	cfg := RBACConfig{
		Roles:       map[string][]string{"viewer": {"read_*"}},
		RoleParents: map[string][]string{"operator": {"viewer"}}, // operator 无直接权限
		UserRoles:   map[string][]string{"u": {"operator"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("仅作为 parent 键的角色应视为已定义: %v", err)
	}
}

// 空配置 → 合法（表示「拒绝一切」，由调用方决定是否允许）。
func TestRBACConfig_Validate_EmptyConfigOK(t *testing.T) {
	if err := (RBACConfig{}).Validate(); err != nil {
		t.Errorf("空配置不应报错: %v", err)
	}
}

// 多个错误应**全部**列出（便于一次修完，而非逐个试）。
func TestRBACConfig_Validate_ReportsAllErrors(t *testing.T) {
	cfg := RBACConfig{
		Roles:     map[string][]string{"admin": {"*"}},
		UserRoles: map[string][]string{"u1": {"admn"}, "u2": {"opertor"}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "admn") || !strings.Contains(msg, "opertor") {
		t.Errorf("错误信息应列出全部未定义角色，got: %v", err)
	}
}
