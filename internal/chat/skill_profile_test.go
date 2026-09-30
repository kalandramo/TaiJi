package chat

import (
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
)

// skill 工具档位的默认值（安全不变量，2026-09-26）。
//
// 背景（实测发现）：不传 SkillToolProfile 时，上游注册 6 个工具，其中含
// workspace_exec / workspace_write_stdin / workspace_kill_session——
// **能执行代码**。而 TaiJi serve 面向 IM 用户、IM 来源是只读上下文
// （internal/server/pipeline.go:290 注入 KindChannel），执行类工具与之冲突。
//
// 实测对比（2026-09-26）：
//
//	不传 profile   (6): + workspace_exec 等 3 个执行类
//	KnowledgeOnly  (3): 仅 skill_load / skill_select_docs / skill_list_docs
//	Full          (11): 再加 skill_run / skill_exec 等
//
// 本测试锁死「空串 → KnowledgeOnly」，防止后人误改回「不传」。

func TestSkillToolProfile_DefaultsToKnowledgeOnly(t *testing.T) {
	for _, in := range []string{"", "  ", "knowledge-only", "Knowledge-Only", "knowledgeonly"} {
		got := skillToolProfile(in)
		if got != llmagent.SkillToolProfileKnowledgeOnly {
			t.Errorf("skillToolProfile(%q) = %q，应为 KnowledgeOnly——"+
				"取上游默认会引入 workspace_exec 等**代码执行**工具，"+
				"与 IM 只读上下文冲突", in, got)
		}
	}
}

func TestSkillToolProfile_ExplicitFull(t *testing.T) {
	for _, in := range []string{"full", "Full", "FULL"} {
		if got := skillToolProfile(in); got != llmagent.SkillToolProfileFull {
			t.Errorf("skillToolProfile(%q) = %q，应为 Full", in, got)
		}
	}
}

// 未知取值透传（上游会校验并报错，不静默降级）。
func TestSkillToolProfile_UnknownPassthrough(t *testing.T) {
	if got := skillToolProfile("something-else"); got != "something-else" {
		t.Errorf("未知取值应透传，got %q", got)
	}
}

// newSkillRepository 的路径处理。
func TestNewSkillRepository(t *testing.T) {
	root := t.TempDir()
	repo, err := newSkillRepository(root)
	if err != nil {
		t.Fatalf("有效路径应成功: %v", err)
	}
	if repo == nil {
		t.Fatal("应返回非 nil 仓库")
	}

	// 全空白 → 报错（而非静默返回空仓库）
	if _, err := newSkillRepository("  "); err == nil {
		t.Error("全空白路径应报错（fail-fast，不静默降级）")
	}
}
