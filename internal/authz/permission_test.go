package authz

import (
	"context"
	"errors"
	"testing"
)

// 静态权限表（方案 A：open_id → 工具白名单）。
//
// 与既有工具策略的分工：
//   - toolpolicy.go（approval 插件）：**部署级**——哪些工具在本部署启用
//   - 本文件（PrincipalPolicy 插件）：**用户级**——谁能用哪些工具
//
// 两者串联：先过部署白名单，再过用户权限。任一拒绝即不执行。

func TestStaticPermissions_ExactMatch(t *testing.T) {
	p := NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"mockmcp_echo"},
	})

	ok, err := p.Allowed(context.Background(), Principal{ID: "ws1:feishu:ou_alice"}, "mockmcp_echo")
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if !ok {
		t.Error("已授权的 (主体, 工具) 应放行")
	}
}

// 未列出的主体 → 拒绝（fail-closed）。
func TestStaticPermissions_UnknownPrincipalDenied(t *testing.T) {
	p := NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"mockmcp_echo"},
	})

	ok, err := p.Allowed(context.Background(), Principal{ID: "ws1:feishu:ou_mallory"}, "mockmcp_echo")
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if ok {
		t.Error("未授权的主体应被拒（fail-closed）")
	}
}

// 主体在表里但工具不在其列表 → 拒绝。
func TestStaticPermissions_ToolNotListedDenied(t *testing.T) {
	p := NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"mockmcp_echo"},
	})

	ok, _ := p.Allowed(context.Background(), Principal{ID: "ws1:feishu:ou_alice"}, "srv_dangerous")
	if ok {
		t.Error("未列出的工具应被拒")
	}
}

// 通配符：srv_* 匹配 srv_echo 但不匹配 other_echo。
func TestStaticPermissions_Wildcard(t *testing.T) {
	p := NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"infraverse_*"},
	})

	ctx := context.Background()
	alice := Principal{ID: "ws1:feishu:ou_alice"}

	if ok, _ := p.Allowed(ctx, alice, "infraverse_dce_ip"); !ok {
		t.Error("infraverse_* 应匹配 infraverse_dce_ip")
	}
	if ok, _ := p.Allowed(ctx, alice, "infraverse_other"); !ok {
		t.Error("infraverse_* 应匹配 infraverse_other")
	}
	if ok, _ := p.Allowed(ctx, alice, "mockmcp_echo"); ok {
		t.Error("infraverse_* 不应匹配 mockmcp_echo")
	}
}

// 全通配 "*" —— 显式授权某用户可用所有已启用工具。
func TestStaticPermissions_StarAllowsAll(t *testing.T) {
	p := NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_admin": {"*"},
	})

	ok, _ := p.Allowed(context.Background(), Principal{ID: "ws1:feishu:ou_admin"}, "anything_at_all")
	if !ok {
		t.Error("\"*\" 应放行任意工具")
	}
}

// 空主体（无身份）→ 拒绝，且不返回 error（这是"查了，不允许"，非"查不了"）。
func TestStaticPermissions_EmptyPrincipalDenied(t *testing.T) {
	p := NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"mockmcp_echo"},
	})

	ok, err := p.Allowed(context.Background(), Principal{}, "mockmcp_echo")
	if err != nil {
		t.Fatalf("不应返回 error（空主体是确定的拒绝，非查询失败）: %v", err)
	}
	if ok {
		t.Error("空主体应被拒（fail-closed）")
	}
}

// nil 权限表 → 全部拒绝（安全基线，与 toolpolicy 的默认拒绝一致）。
func TestStaticPermissions_NilMapDeniesAll(t *testing.T) {
	p := NewStaticPermissions(nil)

	ok, err := p.Allowed(context.Background(), Principal{ID: "ws1:feishu:ou_alice"}, "mockmcp_echo")
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if ok {
		t.Error("nil 权限表应拒绝一切（安全基线）")
	}
}

// error 语义：查询失败 ≠ 不允许。前者应返回 error 让调用方区分日志。
func TestPermissionSource_ErrorMeansUndeterminable(t *testing.T) {
	failing := &failingSource{}
	ok, err := failing.Allowed(context.Background(), Principal{ID: "x"}, "y")
	if err == nil {
		t.Error("查询失败应返回 error")
	}
	if ok {
		t.Error("查询失败不得放行")
	}
	// 调用方应能区分：errors.Is 可用
	if !errors.Is(err, errProbeFailure) {
		t.Errorf("error 应可识别，got %v", err)
	}
}

var errProbeFailure = errors.New("probe: source unavailable")

type failingSource struct{}

func (f *failingSource) Allowed(context.Context, Principal, string) (bool, error) {
	return false, errProbeFailure
}
