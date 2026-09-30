package chat

import (
	"strings"
	"testing"
)

// agent 名必须真正生效（父子关系与多 agent 可观测性的前置）。
//
// 背景：`llmagent.New` 的名字此前是硬编码字面量 "assistant"——所有 agent
// 在框架层同名。后果有二：
//  1. 上游的父子定位 `FindSubAgent(name)`（trpc-agent-go 的
//     agent/agent.go:69-72）无法区分兄弟 agent；
//  2. `transfer_to_agent` 的 agent_name 参数无从填写。
//
// 本测试锁住「名字来自 Options.AgentName」。

func TestExecutor_AgentNameTakesEffect(t *testing.T) {
	ex, err := NewExecutor(Options{
		Config:    modelConfigFor("http://127.0.0.1:1"),
		AppName:   "taiji",
		AgentName: "billing",
		Echo:      &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	defer ex.Close()

	if got := ex.AgentName(); got != "billing" {
		t.Errorf("AgentName() = %q, want billing——"+
			"名字未生效会让 FindSubAgent 无法区分 agent", got)
	}
}

// 向后兼容：AgentName 为空时仍是 "assistant"（既有部署的遥测标签不变）。
func TestExecutor_EmptyAgentNameFallsBackToAssistant(t *testing.T) {
	ex, err := NewExecutor(Options{
		Config:  modelConfigFor("http://127.0.0.1:1"),
		AppName: "taiji",
		// AgentName 故意留空
		Echo: &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	defer ex.Close()

	if got := ex.AgentName(); got != "assistant" {
		t.Errorf("AgentName() = %q, want assistant（向后兼容）", got)
	}
}

// 两个 agent 的名字必须不同——否则 FindSubAgent 有歧义。
func TestExecutor_TwoAgentsHaveDistinctNames(t *testing.T) {
	mk := func(name string) *Executor {
		ex, err := NewExecutor(Options{
			Config:    modelConfigFor("http://127.0.0.1:1"),
			AppName:   "taiji",
			AgentName: name,
			Echo:      &strings.Builder{},
		})
		if err != nil {
			t.Fatalf("NewExecutor(%s): %v", name, err)
		}
		t.Cleanup(ex.Close)
		return ex
	}

	a, b := mk("alpha"), mk("beta")
	if a.AgentName() == b.AgentName() {
		t.Errorf("两个 agent 名字相同（%q），FindSubAgent 会有歧义", a.AgentName())
	}
}
