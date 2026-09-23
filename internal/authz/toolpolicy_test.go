package authz

import (
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// 工具权限策略契约（issue #4 AC）：
//   1. 默认策略必须是 denied —— 新增工具不自动获得执行权
//   2. 白名单中的工具正常放行
//   3. 拒绝以 CustomResult 形式回给模型，不中断 run（由上游保证，见 tool/callbacks.go:77）
//   4. 白名单写了不存在的工具名 → 装配期报错，不静默忽略

// fakeTool 实现 tool.Tool，用于构造"已注册工具"集合。
type fakeTool struct{ name string }

func (f fakeTool) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: f.name}
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Declaration().Name)
	}
	return out
}

func TestBuildToolPolicy_DefaultIsDenied(t *testing.T) {
	// 空白名单 → 默认策略必须是 denied。
	// 这是安全基线：不显式放行的工具一律不能跑。
	registered := []tool.Tool{fakeTool{"echo"}, fakeTool{"dangerous"}}

	p, err := BuildToolPolicy(ToolPolicyConfig{
		Allow:      nil,
		Registered: registered,
	})
	if err != nil {
		t.Fatalf("BuildToolPolicy: %v", err)
	}
	if p == nil {
		t.Fatal("nil plugin")
	}
	if p.DefaultPolicy() != PolicyDenied {
		t.Errorf("default policy = %q, want %q", p.DefaultPolicy(), PolicyDenied)
	}
}

func TestBuildToolPolicy_AllowListIsGranted(t *testing.T) {
	registered := []tool.Tool{fakeTool{"echo"}, fakeTool{"dangerous"}}

	p, err := BuildToolPolicy(ToolPolicyConfig{
		Allow:      []string{"echo"},
		Registered: registered,
	})
	if err != nil {
		t.Fatalf("BuildToolPolicy: %v", err)
	}
	got := p.PolicyFor("echo")
	if got != PolicyAllowed {
		t.Errorf("policy for echo = %q, want %q", got, PolicyAllowed)
	}
	if got := p.PolicyFor("dangerous"); got != PolicyDenied {
		t.Errorf("policy for dangerous = %q, want %q (not in allow list)", got, PolicyDenied)
	}
}

func TestBuildToolPolicy_UnknownToolNameIsReported(t *testing.T) {
	// AC-4：白名单写了不存在的工具名 → 装配期报错，不静默忽略。
	registered := []tool.Tool{fakeTool{"echo"}}

	_, err := BuildToolPolicy(ToolPolicyConfig{
		Allow:      []string{"echo", "no-such-tool"},
		Registered: registered,
	})
	if err == nil {
		t.Fatal("unknown tool name in allow list should be reported")
	}
	if !strings.Contains(err.Error(), "no-such-tool") {
		t.Errorf("error %q should name the unknown tool", err.Error())
	}
	// 已注册的工具名不应被误报
	if strings.Contains(err.Error(), `"echo"`) {
		t.Errorf("error %q should not flag the known tool 'echo'", err.Error())
	}
}

func TestBuildToolPolicy_MultipleUnknownNamesAllReported(t *testing.T) {
	registered := []tool.Tool{fakeTool{"echo"}}

	_, err := BuildToolPolicy(ToolPolicyConfig{
		Allow:      []string{"ghost1", "echo", "ghost2"},
		Registered: registered,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"ghost1", "ghost2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

func TestBuildToolPolicy_EmptyRegisteredSkipsValidation(t *testing.T) {
	// 未提供注册列表时（如无 MCP server），不做存在性校验——
	// 否则会误报所有白名单条目。这是有意的：校验需要真实工具面。
	p, err := BuildToolPolicy(ToolPolicyConfig{
		Allow:      []string{"anything"},
		Registered: nil,
	})
	if err != nil {
		t.Fatalf("with no registered tools, validation should be skipped: %v", err)
	}
	if got := p.PolicyFor("anything"); got != PolicyAllowed {
		t.Errorf("policy = %q, want %q", got, PolicyAllowed)
	}
}

func TestBuildToolPolicy_WhitespaceNamesRejected(t *testing.T) {
	registered := []tool.Tool{fakeTool{"echo"}}

	_, err := BuildToolPolicy(ToolPolicyConfig{
		Allow:      []string{"  ", "echo"},
		Registered: registered,
	})
	if err == nil {
		t.Fatal("blank tool name in allow list should be rejected")
	}
}

func TestToolNames_ExtractsDeclarations(t *testing.T) {
	got := toolNames([]tool.Tool{fakeTool{"a"}, fakeTool{"b"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("toolNames = %v, want [a b]", got)
	}
}
