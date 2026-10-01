package chat

import (
	"os"
	"strings"
	"testing"
)

// 安全不变量：权限守卫必须走 `runner.WithPlugins`（覆盖**所有** agent），
// 而非 `llmagent.WithExtensions`。
//
// 依据（外部依赖 trpc-agent-go 的 agent/llmagent/option.go 的注释）：
//
//	Extensions installed here are scoped to this LLMAgent only. They do
//	NOT propagate to sub-agents... use plugin.Plugin via
//	runner.WithPlugins for cross-cutting concerns that must observe
//	**every agent on a runner**.
//
// 换句话说：改用 WithExtensions 会让**子 agent 失去权限检查**——
// 而子 agent 是模型可通过 transfer_to_agent 主动选择的，那等于
// 开了一条绕过权限的通道（transfer 是"框架工具"，白名单也拦不住）。
//
// 本测试用源码级断言锁住接线（与 allowtools_test.go / serve_permissions
// 的接线断言同风格）。

func TestPermissionGuard_UsesRunnerPlugins(t *testing.T) {
	data, err := os.ReadFile("execute.go")
	if err != nil {
		t.Fatalf("读取 execute.go: %v", err)
	}
	code := stripComments(string(data))

	// 1) 必须在 runner 上挂插件（而非只在单个 llmagent 上）。
	if !strings.Contains(code, "runner.WithPlugins(") {
		t.Error("execute.go 未使用 runner.WithPlugins——" +
			"权限守卫将无法覆盖子 agent（可以用 WithExtensions 证实这一点）")
	}

	// 2) 权限插件必须在那组插件里。
	if !strings.Contains(code, "NewPrincipalPolicyPlugin(") {
		t.Error("未装配 NewPrincipalPolicyPlugin——用户级权限判定缺失")
	}
	if !strings.Contains(code, "NewContextGuardPlugin(") {
		t.Error("未装配 NewContextGuardPlugin——上下文只读降维缺失")
	}

	// 3) 反面：不得用 WithExtensions 装权限（它不传播到子 agent）。
	if strings.Contains(code, "llmagent.WithExtensions(") {
		t.Error("检测到 llmagent.WithExtensions——若权限走这条路，" +
			"子 agent 将失去权限检查（用 runner.WithPlugins 代替）")
	}
}

// stripComments 移除行注释，避免注释里的字符串造成假绿
// （实测教训见 serve_permissions_test.go 的反证记录）。
func stripComments(s string) string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if i := strings.Index(ln, "//"); i >= 0 {
			ln = ln[:i]
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
