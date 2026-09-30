package authz

import (
	"context"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// skill 读类工具在渠道上下文（飞书）的放行（缺口：上游未标 ReadOnly）。
//
// 背景（实测确认，2026-09-26）：
//   上游 v1.11.2 的 skill 工具**未实现 tool.MetadataProvider**
//   （`grep -rln ToolMetadata` 对 tool/skill/ 返回空），
//   故 `tool.MetadataOf` 返回零值 → ReadOnly=false。
//   实测：KindChannel 下 skill_load / skill_list_docs / skill_select_docs
//   **全部被 ContextGuard 拒绝**（日志："write tool in non-writable context"）。
//
// 但三者事实只读：
//   - skill_load        —— 把 SKILL.md 正文读进模型上下文
//   - skill_list_docs   —— 列出 skill 的文档
//   - skill_select_docs —— 只改**会话内**的文档选择状态（读 ctx，无外部副作用）
//
// 故在守卫构造时按**精确名**修正其只读性。
//
// **绝不能按 `skill_` 前缀通配**：同家族的 skill_run / skill_exec 是
// **真写操作**（执行代码），通配会在切到 SkillToolProfileFull 时把它们
// 一并放行——开的洞比要修的问题更大。本测试的最后一个用例锁死这点。

// 三个读类工具在渠道上下文应放行。
func TestContextGuard_SkillReadToolsAllowedInChannel(t *testing.T) {
	for _, name := range []string{"skill_load", "skill_list_docs", "skill_select_docs"} {
		t.Run(name, func(t *testing.T) {
			// 模拟上游：未声明 ReadOnly（MetadataOf 返回零值）
			g := buildGuard(t, metaTool{name: name, md: tool.ToolMetadata{}})
			ctx := WithContextKind(context.Background(), KindChannel)

			res, err := runGuard(t, g, ctx, name)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res != nil && res.CustomResult != nil {
				t.Errorf("skill 读类工具 %s 在渠道上下文应放行，"+
					"实际被拒：%v（上游未标 ReadOnly，需在此修正）",
					name, res.CustomResult)
			}
		})
	}
}

// skill 执行类工具必须**仍被拒绝**——防止前缀通配误放行。
//
// 这是修法的安全边界：只放行三个读类工具，不放行整个 skill_ 家族。
func TestContextGuard_SkillExecToolsStillDeniedInChannel(t *testing.T) {
	for _, name := range []string{
		"skill_run", "skill_exec", "skill_write_stdin",
		"skill_poll_session", "skill_kill_session", "workspace_exec",
	} {
		t.Run(name, func(t *testing.T) {
			g := buildGuard(t, metaTool{name: name, md: tool.ToolMetadata{}})
			ctx := WithContextKind(context.Background(), KindChannel)

			res, err := runGuard(t, g, ctx, name)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res == nil || res.CustomResult == nil {
				t.Errorf("skill 执行类工具 %s 是写操作，渠道上下文必须拒绝，"+
					"实际放行——修法用了前缀通配？", name)
			}
		})
	}
}

// 未知工具（含伪造的 skill_ 前缀名）仍 fail-closed。
func TestContextGuard_FakeSkillNameStillDenied(t *testing.T) {
	// 例如上游将来新增 skill_dangerous，不该被自动放行。
	g := buildGuard(t, metaTool{name: "skill_dangerous_new", md: tool.ToolMetadata{}})
	ctx := WithContextKind(context.Background(), KindChannel)

	res, err := runGuard(t, g, ctx, "skill_dangerous_new")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.CustomResult == nil {
		t.Error("未在显式名单中的 skill_* 工具应 fail-closed 拒绝，" +
			"实际放行——说明用了前缀匹配而非精确名")
	}
}

// 非 skill 工具的既有语义不受影响（回归护栏）。
func TestContextGuard_NonSkillSemanticsUnchanged(t *testing.T) {
	// 写工具仍拒
	g := buildGuard(t, metaTool{name: "write_note", md: tool.ToolMetadata{ReadOnly: false}})
	ctx := WithContextKind(context.Background(), KindChannel)
	if res, _ := runGuard(t, g, ctx, "write_note"); res == nil || res.CustomResult == nil {
		t.Error("普通写工具应仍被拒")
	}

	// 显式声明只读的仍放行
	g2 := buildGuard(t, metaTool{name: "echo", md: tool.ToolMetadata{ReadOnly: true}})
	if res, _ := runGuard(t, g2, ctx, "echo"); res != nil && res.CustomResult != nil {
		t.Error("显式只读工具应仍放行")
	}
}
