package authz

import (
	"context"
	"testing"
)

// 权限的 agent 维度（N5 权限隔离）。
//
// 需求：「同一用户在 agent A 有权限、在 agent B 无权限」。
//
// 设计（与 AccessRequest.Resource 同构，v1 透传、v2 参与判定）：
//   - `AccessRequest.Agent` 为空 ⇒ 单 agent 部署，行为与改动前**完全一致**。
//   - 裸模式（`mockmcp_echo` / `srv_*`）⇒ **全局**，对所有 agent 生效。
//   - `agent:{name}:{toolPattern}` ⇒ 仅当 Agent == name 时生效。
//
// **裸模式必须是全局**（而非「仅单 agent」）：否则任何部署一旦启用多
// agent，现有配置会静默失去全部权限——那是比缺功能严重得多的破坏。

func agentRBAC(t *testing.T) *RBACPermissions {
	t.Helper()
	return NewRBACPermissions(RBACConfig{
		Roles: map[string][]string{
			// 全局工具：所有 agent 都可用
			"base": {"glob_tool"},
			// 运维专属：仅 ops agent 可用
			"ops_only": {"agent:ops:handbook_*"},
			// 账单专属：仅 billing agent 可用
			"bill_only": {"agent:billing:invoice_read"},
		},
		UserRoles: map[string][]string{"u": {"base", "ops_only", "bill_only"}},
	})
}

func TestRBAC_EmptyAgentKeepsLegacyBehaviour(t *testing.T) {
	p := agentRBAC(t)
	ctx := context.Background()
	u := Principal{ID: "u"}

	// 裸模式：Agent 为空时照常匹配（向后兼容）。
	if ok, _ := p.Allowed(ctx, AccessRequest{
		Principal: u, Action: "glob_tool",
	}); !ok {
		t.Error("Agent 为空时裸模式应放行（向后兼容）")
	}

	// agent 限定模式在无 agent 维度时**不匹配**——没有 agent 可判。
	if ok, _ := p.Allowed(ctx, AccessRequest{
		Principal: u, Action: "handbook_x",
	}); ok {
		t.Error("Agent 为空时 agent 限定模式不应匹配（无 agent 可判）")
	}
}

func TestRBAC_PlainPatternIsGlobal(t *testing.T) {
	p := agentRBAC(t)
	ctx := context.Background()
	u := Principal{ID: "u"}

	// 关键：多 agent 模式下，裸模式对所有 agent 生效
	// （否则启用多 agent 会静默失去现有权限）。
	for _, agent := range []string{"ops", "billing", "other"} {
		if ok, _ := p.Allowed(ctx, AccessRequest{
			Principal: u, Action: "glob_tool", Agent: agent,
		}); !ok {
			t.Errorf("裸模式应对 agent %q 生效（全局语义）", agent)
		}
	}
}

func TestRBAC_AgentScopedPatternExpressesIsolation(t *testing.T) {
	p := agentRBAC(t)
	ctx := context.Background()
	u := Principal{ID: "u"}

	cases := []struct {
		agent  string
		action string
		want   bool
		why    string
	}{
		{"ops", "handbook_read", true, "ops 专属模式，agent 匹配 → 放行"},
		{"ops", "handbook_write", true, "前缀通配生效"},
		{"billing", "handbook_read", false, "**核心**：billing 不得用 ops 的权限"},
		{"billing", "invoice_read", true, "billing 专属"},
		{"ops", "invoice_read", false, "反向：ops 不得用 billing 的权限"},
		{"other", "handbook_read", false, "未声明的 agent 不得用"},
	}
	for _, c := range cases {
		got, _ := p.Allowed(ctx, AccessRequest{
			Principal: u, Action: c.action, Agent: c.agent,
		})
		if got != c.want {
			t.Errorf("Agent=%s Action=%s: got=%v want=%v（%s）",
				c.agent, c.action, got, c.want, c.why)
		}
	}
}

// 形态非法的 agent 限定模式不应匹配（fail-closed）。
func TestRBAC_MalformedAgentPatternDenied(t *testing.T) {
	p := NewRBACPermissions(RBACConfig{
		Roles:     map[string][]string{"r": {"agent:ops"}}, // 缺第三段
		UserRoles: map[string][]string{"u": {"r"}},
	})
	// 该模式形态非法 → 任何 agent 下都不匹配（fail-closed）。
	// 注意：Validate 会先检查角色引用，此处只验匹配逻辑。
	ok, _ := p.Allowed(context.Background(), AccessRequest{
		Principal: Principal{ID: "u"}, Action: "anything", Agent: "ops",
	})
	if ok {
		t.Error("形态非法的 agent 模式不应匹配（fail-closed）")
	}
}

// StaticPermissions 与 RBAC 共用同一匹配语义（避免两套判定漂移）。
func TestStaticPermissions_SupportsAgentScope(t *testing.T) {
	p := NewStaticPermissions(map[string][]string{
		"u": {"glob_tool", "agent:ops:handbook_*"},
	})
	ctx := context.Background()
	u := Principal{ID: "u"}

	if ok, _ := p.Allowed(ctx, AccessRequest{
		Principal: u, Action: "glob_tool", Agent: "billing",
	}); !ok {
		t.Error("裸模式应全局生效（StaticPermissions 亦同）")
	}
	if ok, _ := p.Allowed(ctx, AccessRequest{
		Principal: u, Action: "handbook_read", Agent: "ops",
	}); !ok {
		t.Error("agent 限定模式应生效")
	}
	if ok, _ := p.Allowed(ctx, AccessRequest{
		Principal: u, Action: "handbook_read", Agent: "billing",
	}); ok {
		t.Error("agent 限定模式不得跨 agent 生效")
	}
}
