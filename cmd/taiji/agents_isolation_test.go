package main

import (
	"strings"
	"testing"
)

// 每 agent 隔离配置的解析（N1–N5）。
//
// TAIJI_AGENTS 扩展字段（全部可选）：
//
//	parent=<name>         父子关系（N1）
//	instruction=<text>    该 agent 的系统提示（N2）
//	skills=<path>         skill 仓库根（N3）
//	allow_tools=<a,b>     MCP 工具白名单（N4）
//	roles=<r1,r2>         权限角色（N5）

func TestParseAgents_PerAgentInstruction(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=a,app_id=cli_1,app_secret=s1,instruction=你是账单助手;"+
			"name=b,app_id=cli_2,app_secret=s2,instruction=你是运维助手")

	specs, err := parseAgents()
	if err != nil {
		t.Fatalf("parseAgents: %v", err)
	}
	byName := map[string]agentSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	if byName["a"].Instruction != "你是账单助手" {
		t.Errorf("a.Instruction = %q", byName["a"].Instruction)
	}
	if byName["b"].Instruction != "你是运维助手" {
		t.Errorf("b.Instruction = %q", byName["b"].Instruction)
	}
	// 关键：两者必须不同（隔离的本质）。
	if byName["a"].Instruction == byName["b"].Instruction {
		t.Error("两个 agent 的 instruction 相同——未隔离")
	}
}

func TestParseAgents_PerAgentSkills(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=a,app_id=cli_1,app_secret=s1,skills=/opt/skills/a;"+
			"name=b,app_id=cli_2,app_secret=s2")

	specs, _ := parseAgents()
	byName := map[string]agentSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	if byName["a"].Skills != "/opt/skills/a" {
		t.Errorf("a.Skills = %q", byName["a"].Skills)
	}
	if byName["b"].Skills != "" {
		t.Errorf("b.Skills 应留空（用全局默认），got %q", byName["b"].Skills)
	}
}

func TestParseAgents_PerAgentAllowTools(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=a,app_id=cli_1,app_secret=s1,allow_tools=mcp_x_ping|mcp_x_echo")

	specs, _ := parseAgents()
	if len(specs) != 1 {
		t.Fatalf("应解析 1 个 agent，got %d", len(specs))
	}
	got := specs[0].AllowTools
	if len(got) != 2 || got[0] != "mcp_x_ping" || got[1] != "mcp_x_echo" {
		t.Errorf("AllowTools = %v，应为 [mcp_x_ping mcp_x_echo]", got)
	}
}

func TestParseAgents_ParentDeclared(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=root,app_id=cli_1,app_secret=s1;"+
			"name=leaf,app_id=cli_2,app_secret=s2,parent=root")

	specs, _ := parseAgents()
	byName := map[string]agentSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	if byName["leaf"].Parent != "root" {
		t.Errorf("leaf.Parent = %q, want root", byName["leaf"].Parent)
	}
	if byName["root"].Parent != "" {
		t.Errorf("root.Parent 应为空，got %q", byName["root"].Parent)
	}
}

// 声明不存在的父 → fail-fast（否则子永远无法被调用）。
func TestParseAgents_UnknownParentFailsFast(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=leaf,app_id=cli_1,app_secret=s1,parent=nonexistent")

	_, err := parseAgents()
	if err == nil {
		t.Fatal("父不存在应报错（fail-fast）")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("错误信息应指出未定义的父名，got: %v", err)
	}
}

// **成环** → fail-fast（拓扑序装配会无限递归）。
func TestParseAgents_CycleFailsFast(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=a,app_id=cli_1,app_secret=s1,parent=b;"+
			"name=b,app_id=cli_2,app_secret=s2,parent=a")

	_, err := parseAgents()
	if err == nil {
		t.Fatal("父子成环应报错（fail-fast）——拓扑序装配会无限递归")
	}
	if !strings.Contains(err.Error(), "环") && !strings.Contains(err.Error(), "cycle") {
		t.Errorf("错误信息应指出成环，got: %v", err)
	}
}

// 自环也要报错。
func TestParseAgents_SelfParentFailsFast(t *testing.T) {
	t.Setenv("TAIJI_AGENTS", "name=a,app_id=cli_1,app_secret=s1,parent=a")
	if _, err := parseAgents(); err == nil {
		t.Fatal("自环应报错")
	}
}

// 向后兼容：未配新字段时行为不变。
func TestParseAgents_NewFieldsOptional(t *testing.T) {
	t.Setenv("TAIJI_AGENTS", "name=a,app_id=cli_1,app_secret=s1")
	specs, err := parseAgents()
	if err != nil {
		t.Fatalf("不带新字段应正常解析: %v", err)
	}
	if specs[0].Instruction != "" || specs[0].Skills != "" ||
		specs[0].Parent != "" || len(specs[0].AllowTools) != 0 {
		t.Errorf("新字段应留空，got %+v", specs[0])
	}
}

// instruction 可含空格与中文（值取自 = 之后到字段结束）。
func TestParseAgents_InstructionWithSpaces(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=a,app_id=cli_1,app_secret=s1,instruction=你是 一个 助手")
	specs, _ := parseAgents()
	if specs[0].Instruction != "你是 一个 助手" {
		t.Errorf("Instruction = %q（内部空格应保留）", specs[0].Instruction)
	}
}
