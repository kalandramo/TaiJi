package chat

import (
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
)

// 父子 agent 装配（N1）。
//
// 上游语义（trpc-agent-go）：
//   - `llmagent.WithSubAgents([]agent.Agent)` 把子 agent 挂到父的树上
//   - 父的 Tools() 会**追加一个** transfer_to_agent 工具（不是每子一个），
//     模型在其中通过 agent_name 选目标
//
// 测试用 llmagent.New 直接造子 agent（无需 model）——它们只被读取
// （Info/Tools），不被运行。

func TestExecutor_SubAgentsWired(t *testing.T) {
	childA := llmagent.New("child-a")
	childB := llmagent.New("child-b")

	ex, err := NewExecutor(Options{
		Config:    modelConfigFor("http://127.0.0.1:1"),
		AppName:   "taiji",
		AgentName: "parent",
		SubAgents: []agent.Agent{childA, childB},
		Echo:      &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	defer ex.Close()

	subs := ex.Agent().SubAgents()
	if len(subs) != 2 {
		t.Fatalf("父应有 2 个子 agent，got %d", len(subs))
	}

	// 按名可查（上游 FindSubAgent 依赖名字唯一）。
	if got := ex.Agent().FindSubAgent("child-a"); got == nil {
		t.Error("FindSubAgent(child-a) 应找到——Wave 1 的 agent 名生效是它的前提")
	}
	if got := ex.Agent().FindSubAgent("nonexistent"); got != nil {
		t.Error("不存在的子不应被找到")
	}
}

// 父的工具面必须含 transfer_to_agent（上游由 WithSubAgents 自动追加）。
func TestExecutor_ParentGainsTransferTool(t *testing.T) {
	ex, err := NewExecutor(Options{
		Config:    modelConfigFor("http://127.0.0.1:1"),
		AppName:   "taiji",
		AgentName: "parent",
		SubAgents: []agent.Agent{llmagent.New("child")},
		Echo:      &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	defer ex.Close()

	hasTransfer := false
	for _, n := range ex.RegisteredTools() {
		if n == "transfer_to_agent" {
			hasTransfer = true
		}
	}
	if !hasTransfer {
		t.Errorf("有子 agent 时父的工具面应含 transfer_to_agent，got %v",
			ex.RegisteredTools())
	}
}

// 无子 agent 时不得出现 transfer 工具（回归护栏）。
func TestExecutor_NoSubAgentsNoTransferTool(t *testing.T) {
	ex, err := NewExecutor(Options{
		Config:    modelConfigFor("http://127.0.0.1:1"),
		AppName:   "taiji",
		AgentName: "solo",
		Echo:      &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	defer ex.Close()

	for _, n := range ex.RegisteredTools() {
		if n == "transfer_to_agent" {
			t.Error("无子 agent 不应出现 transfer_to_agent")
		}
	}
}
