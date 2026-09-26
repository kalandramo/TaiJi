package authz

import (
	"context"
	"fmt"
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

// 拒绝文案必须含「不要重试」指令（实测缺陷修复）。
//
// 用户实测（2026-09-24 22:59）：模型被拒后**连试 3 次**同一个工具，
// 日志出现 3 条 [perm] denied，最终回复是三次失败后的道歉。
//
// 根因：模型把 CustomResult 当普通工具返回值，认为换个说法重试可能成功。
// 但权限判定是确定性的——重试不会改变结果。文案必须明说这点。
//
// 这条测试锁定文案的**语义要素**，不是逐字比对：
// 只要含「不要/请勿重试」这类指令就算通过，允许后续润色措辞。
func TestPrincipalPolicy_DenyMessageTellsModelNotToRetry(t *testing.T) {
	plugin := NewPrincipalPolicyPlugin(NewStaticPermissions(map[string][]string{
		"ws1:feishu:ou_alice": {"mockmcp_echo"},
	}), nil)

	ctx := WithPrincipal(context.Background(), Principal{ID: "ws1:feishu:ou_bob"})
	got := runBeforeTool(t, plugin, ctx, "mockmcp_echo")

	if got == nil || got.CustomResult == nil {
		t.Fatal("应被拒")
	}
	msg, _ := got.CustomResult.(string)

	// 必须明确告知「重试无用」——否则模型会反复调用同一工具
	if !strings.Contains(msg, "重试") {
		t.Errorf("拒绝文案应明确告知重试无效（模型会连试多次），got %q", msg)
	}
	// 必须指出这是确定性拒绝（不是暂时性故障）
	if !strings.Contains(msg, "确定") {
		t.Errorf("拒绝文案应说明是确定性拒绝（区别于临时故障），got %q", msg)
	}
	// 应给出路（向用户说明需授权），而不是让模型卡在重试循环
	if !strings.Contains(msg, "管理员") && !strings.Contains(msg, "授权") {
		t.Errorf("拒绝文案应给出路（如请管理员授权），got %q", msg)
	}
	t.Logf("✓ 拒绝文案含停止指令: %q", msg)
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

// 拒绝日志含审计三要素（AC-5：principal + action + resource）。
//
// 动机：审计要求「谁 / 做什么 / 对什么」。resource 是 v1 新增的上下文
// 维度（工作区 ID），若不进日志则审计缺一半。
func TestPrincipalPolicy_DenyLogIncludesResource(t *testing.T) {
	var logs []string
	plugin := NewPrincipalPolicyPlugin(
		NewStaticPermissions(nil), // 拒绝一切
		func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	)

	ctx := WithPrincipal(context.Background(), Principal{ID: "ws1:feishu:ou_alice"})
	ctx = WithResource(ctx, "ws-42")

	runBeforeTool(t, plugin, ctx, "mockmcp_echo")

	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "ws-42") {
		t.Errorf("拒绝日志应含 resource（ws-42），got: %s", joined)
	}
	if !strings.Contains(joined, "ou_alice") && !strings.Contains(joined, "ws1:feishu") {
		t.Errorf("拒绝日志应含 principal 标识，got: %s", joined)
	}
	if !strings.Contains(joined, "mockmcp_echo") {
		t.Errorf("拒绝日志应含 action（工具名），got: %s", joined)
	}
}
