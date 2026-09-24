package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
)

// serve 路径的工具白名单（issue #9 AC-2 的接线缺口回归测试）。
//
// 背景：工具策略是「默认拒绝 + 白名单放行」（issue #4）。serve 路径曾漏传
// AllowTools，导致配了 MCP server 也永远无法执行——模型看得见工具、调用被
// 策略拒，且无任何报错。真实平台表现为「问工具类问题，bot 答『无法获取』」。
//
// 根因证据（探针实测，2026-09）：
//
//	场景A（serve 路径，无 AllowTools）: tool=mockmcp_echo policy=denied
//	场景B（CLI  路径，有 AllowTools）: tool=mockmcp_echo policy=allowed

func TestEnvAllowTools_EmptyIsNil(t *testing.T) {
	t.Setenv("TAIJI_ALLOW_TOOLS", "")
	if got := envAllowTools(); got != nil {
		t.Errorf("envAllowTools() with empty env = %v, want nil", got)
	}
}

func TestEnvAllowTools_CommaSeparated(t *testing.T) {
	t.Setenv("TAIJI_ALLOW_TOOLS", "mockmcp_echo, srvA_echo ,srvB_echo")
	got := envAllowTools()
	want := []string{"mockmcp_echo", "srvA_echo", "srvB_echo"}
	if len(got) != len(want) {
		t.Fatalf("envAllowTools() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("envAllowTools()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEnvAllowTools_SkipsBlankEntries(t *testing.T) {
	// "a,,b" 与 " a , , b " 都不应产生空条目——空白名单条目会在
	// BuildToolPolicy 里被当作错误拒绝（"blank tool name in allow list"）。
	t.Setenv("TAIJI_ALLOW_TOOLS", "a,,  ,b")
	got := envAllowTools()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("envAllowTools() = %v, want [a b]", got)
	}
}

func TestEnvAllowTools_WhitespaceOnlyIsNil(t *testing.T) {
	t.Setenv("TAIJI_ALLOW_TOOLS", "   ")
	if got := envAllowTools(); got != nil {
		t.Errorf("envAllowTools() with whitespace-only = %v, want nil", got)
	}
}

// buildPipeline 必须把白名单交给执行器。
//
// 这条测试守的是**接线**而非解析：解析对了但没传下去，缺口依旧。
// 由于 buildPipeline 需要真实模型/MCP 装配，这里用源码级断言——
// 确认 AllowTools 出现在 NewExecutor 的实参里。
//
// 为什么不用运行时断言：装配 Executor 需要可用的模型端点，成本高且脆弱；
// 而这里要守的是「有没有传」这一条静态事实。
func TestBuildPipeline_PassesAllowToolsToExecutor(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	// 定位 buildPipeline 内的 NewExecutor 调用段
	start := indexOf(text, "func buildPipeline(")
	if start < 0 {
		t.Fatal("buildPipeline not found")
	}
	end := indexOf(text[start:], "\nfunc ")
	if end < 0 {
		end = len(text) - start
	}
	body := text[start : start+end]

	if !contains(body, "AllowTools:") {
		t.Error("buildPipeline 的 chat.Options 缺少 AllowTools——" +
			"工具策略默认拒绝，漏传会让所有工具调用被拒（AC-2 缺口）")
	}
	if !contains(body, "envAllowTools()") {
		t.Error("buildPipeline 未调用 envAllowTools()——白名单无来源")
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

// 语义验证：envAllowTools 的产出喂给策略后，列出的工具必须真的被放行。
//
// 这是比字符串断言更强的一层——它走的是真实的解析 → 策略装配链路，
// 证明「白名单非空」确实等价于「工具可执行」，而不是仅仅「变量被传下去」。
func TestEnvAllowTools_ResultsInAllowedPolicy(t *testing.T) {
	t.Setenv("TAIJI_ALLOW_TOOLS", "mockmcp_echo,srvA_echo")

	policy, err := authz.BuildToolPolicy(authz.ToolPolicyConfig{
		Allow: envAllowTools(),
		// Registered 为 nil：跳过存在性校验（此处不关心工具是否已注册）
	})
	if err != nil {
		t.Fatalf("BuildToolPolicy: %v", err)
	}

	for _, name := range []string{"mockmcp_echo", "srvA_echo"} {
		if got := policy.PolicyFor(name); got != authz.PolicyAllowed {
			t.Errorf("PolicyFor(%q) = %v, want allowed", name, got)
		}
	}
	// 未列出的工具仍必须是拒绝——白名单不能意外放宽。
	if got := policy.PolicyFor("srvB_echo"); got != authz.PolicyDenied {
		t.Errorf("PolicyFor(%q) = %v, want denied", "srvB_echo", got)
	}
}

// 反面对照：白名单为空（serve 路径的旧行为）→ 所有工具被拒。
// 这条断言解释了 AC-2 为何在真实平台失败。
func TestEnvAllowTools_EmptyResultsInDeniedPolicy(t *testing.T) {
	t.Setenv("TAIJI_ALLOW_TOOLS", "")

	policy, err := authz.BuildToolPolicy(authz.ToolPolicyConfig{Allow: envAllowTools()})
	if err != nil {
		t.Fatalf("BuildToolPolicy: %v", err)
	}
	if got := policy.PolicyFor("mockmcp_echo"); got != authz.PolicyDenied {
		t.Errorf("PolicyFor with empty allowlist = %v, want denied "+
			"(这正是 serve 路径漏传 AllowTools 时的症状)", got)
	}
}
