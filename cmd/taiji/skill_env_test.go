package main

import (
	"strings"
	"testing"
)

// serve 路径 skill 与指令环境变量的读取（2026-09-26）。
//
// 背景：serve 此前**没有** Instruction 通道（CLI 有 -instruction flag）。
// 本组测试锁住新通道的语义，并防止它在后续重构中被静默移除。

func TestEnvInstruction(t *testing.T) {
	t.Setenv("TAIJI_INSTRUCTION", "")
	if got := envInstruction(); got != "" {
		t.Errorf("未配置应返回空串，got %q", got)
	}

	t.Setenv("TAIJI_INSTRUCTION", "  你是太极助手  ")
	if got := envInstruction(); got != "你是太极助手" {
		t.Errorf("应 TrimSpace，got %q", got)
	}
}

func TestEnvSkillRoot(t *testing.T) {
	t.Setenv("TAIJI_SKILLS_ROOT", "")
	if got := envSkillRoot(); got != "" {
		t.Errorf("未配置应返回空串（不启用 skill），got %q", got)
	}

	t.Setenv("TAIJI_SKILLS_ROOT", "/opt/skills")
	if got := envSkillRoot(); got != "/opt/skills" {
		t.Errorf("got %q", got)
	}
}

func TestEnvSkillToolProfile(t *testing.T) {
	t.Setenv("TAIJI_SKILL_TOOL_PROFILE", "")
	if got := envSkillToolProfile(); got != "" {
		t.Errorf("未配置应返回空串（用上游默认），got %q", got)
	}

	t.Setenv("TAIJI_SKILL_TOOL_PROFILE", "knowledge-only")
	if got := envSkillToolProfile(); got != "knowledge-only" {
		t.Errorf("got %q", got)
	}
}

// 接线断言：serve 装配必须真的传这三个字段。
//
// 理由：这三个字段此前不存在，若只加 env 读取函数而忘记接线，
// 功能静默不生效（CLI 与 serve 的分叉正是这么来的）。
func TestBuildPipeline_WiresSkillAndInstruction(t *testing.T) {
	body := functionBody(t, "buildPipeline")
	code := stripLineComments(body)

	for _, want := range []string{
		"Instruction:",
		"SkillRoot:",
		"envInstruction()",
		"envSkillRoot()",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("buildPipeline 未接线 %s——功能会静默不生效", want)
		}
	}
}
