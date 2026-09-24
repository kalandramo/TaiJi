package authz

import (
	"context"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// PrincipalPolicy 插件：在工具执行前按用户权限判定。
//
// 消费 ctx 里的 Principal（由管道注入，见 server/pipeline.go），
// 与部署级 approval 插件串联。

// 授权用户 → 放行（返回 nil，不干扰执行）。
func TestPrincipalPolicy_AllowedPasses(t *testing.T) {
	plugin := NewPrincipalPolicyPlugin(NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"mockmcp_echo"},
	}), nil)

	ctx := WithPrincipal(context.Background(), Principal{ID: "ws1:feishu:ou_alice"})
	got := runBeforeTool(t, plugin, ctx, "mockmcp_echo")

	if got != nil && got.CustomResult != nil {
		t.Errorf("已授权应放行，但被拒: %v", got.CustomResult)
	}
}

// 未授权用户 → 拒绝（CustomResult 非 nil 会跳过工具执行）。
func TestPrincipalPolicy_DeniedBlocks(t *testing.T) {
	plugin := NewPrincipalPolicyPlugin(NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"mockmcp_echo"},
	}), nil)

	ctx := WithPrincipal(context.Background(), Principal{ID: "ws1:feishu:ou_bob"})
	got := runBeforeTool(t, plugin, ctx, "mockmcp_echo")

	if got == nil || got.CustomResult == nil {
		t.Fatal("未授权用户应被拒（CustomResult 非 nil）")
	}
	// 拒绝文案应可读（会进最终回复给用户看）
	msg, _ := got.CustomResult.(string)
	if msg == "" {
		t.Error("拒绝文案不应为空")
	}
	if !strings.Contains(msg, "权限") && !strings.Contains(msg, "permission") {
		t.Errorf("拒绝文案应说明原因，got %q", msg)
	}
}

// 无身份（ctx 里没有 Principal）→ 拒绝，且必须记录（不能静默）。
//
// 这是 fail-closed 的核心：不知道是谁就不放行。
func TestPrincipalPolicy_MissingPrincipalDenied(t *testing.T) {
	var logged []string
	plugin := NewPrincipalPolicyPlugin(NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"*"},
	}), func(f string, a ...any) {
		logged = append(logged, f)
	})

	got := runBeforeTool(t, plugin, context.Background(), "mockmcp_echo")

	if got == nil || got.CustomResult == nil {
		t.Fatal("无身份应被拒（fail-closed）")
	}
	if len(logged) == 0 {
		t.Error("无身份被拒时应记录日志——否则静默失效难排查")
	}
}

// nil 权限源 → 拒绝一切（安全基线）。
func TestPrincipalPolicy_NilSourceDeniesAll(t *testing.T) {
	plugin := NewPrincipalPolicyPlugin(nil, nil)

	ctx := WithPrincipal(context.Background(), Principal{ID: "ws1:feishu:ou_alice"})
	got := runBeforeTool(t, plugin, ctx, "mockmcp_echo")

	if got == nil || got.CustomResult == nil {
		t.Error("nil 权限源应拒绝一切")
	}
}

// 权限源返回 error → 拒绝，且日志区分"查不了"与"不允许"。
func TestPrincipalPolicy_SourceErrorDeniedAndLogged(t *testing.T) {
	var logged []string
	plugin := NewPrincipalPolicyPlugin(&failingSource{}, func(f string, a ...any) {
		logged = append(logged, f)
	})

	ctx := WithPrincipal(context.Background(), Principal{ID: "ws1:feishu:ou_alice"})
	got := runBeforeTool(t, plugin, ctx, "mockmcp_echo")

	if got == nil || got.CustomResult == nil {
		t.Fatal("权限源故障时应拒绝（fail-closed）")
	}
	joined := strings.Join(logged, "|")
	if !strings.Contains(joined, "unavailable") && !strings.Contains(joined, "查询") && !strings.Contains(joined, "source") {
		t.Errorf("日志应体现'查不了'而非'不允许'，got %v", logged)
	}
}

// runBeforeTool 取出插件的 BeforeTool 回调并执行（模拟框架行为）。
func runBeforeTool(t *testing.T, p *PrincipalPolicyPlugin, ctx context.Context, toolName string) *tool.BeforeToolResult {
	t.Helper()
	cb := p.beforeTool()
	res, err := cb(ctx, &tool.BeforeToolArgs{ToolName: toolName})
	if err != nil {
		t.Fatalf("beforeTool 返回 error: %v", err)
	}
	return res
}
