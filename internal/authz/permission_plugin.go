package authz

import (
	"context"
	"fmt"

	"trpc.group/trpc-go/trpc-agent-go/plugin"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// PrincipalPolicyPlugin 在工具执行前按**用户权限**判定。
//
// 与 approval 插件（toolpolicy.go）串联，各管一个维度：
//
//	approval          部署级——哪些工具在本部署启用（启动期定死）
//	PrincipalPolicy   用户级——谁能用哪些工具（读 ctx 里的 Principal）
//
// 为什么是独立插件而非扩展 approval：approval 是上游 SDK 实现，
// 改它意味着 fork；且两个维度的变更频率不同（部署配置 vs 用户权限）。
//
// 判定数据来自 ctx 里的 Principal——由管道注入（server/pipeline.go）。
// 实测确认框架会把 runCtx 透传到工具回调。
type PrincipalPolicyPlugin struct {
	source PermissionSource
	// logf 记录拒绝原因，可为 nil。**拒绝必须留痕**——否则"按用户管控"
	// 会在无声中失效，排查时无从下手。
	logf func(format string, args ...any)
}

// NewPrincipalPolicyPlugin 构造插件。
//
// source 为 nil 时拒绝一切（安全基线，与 toolpolicy 的默认拒绝一致）。
func NewPrincipalPolicyPlugin(source PermissionSource, logf func(string, ...any)) *PrincipalPolicyPlugin {
	return &PrincipalPolicyPlugin{source: source, logf: logf}
}

// Name 实现 plugin.Plugin。名字用于区分多个插件实例。
func (p *PrincipalPolicyPlugin) Name() string { return "principal_policy" }

// Register 实现 plugin.Plugin。
func (p *PrincipalPolicyPlugin) Register(r *plugin.Registry) {
	if r == nil {
		return
	}
	r.BeforeTool(p.beforeTool())
}

// beforeTool 是判定入口。
//
// 拒绝方式：返回 CustomResult —— 工具不执行，但不中断 run，
// 模型收到该结果后可继续对话（与 approval 插件一致）。
func (p *PrincipalPolicyPlugin) beforeTool() tool.BeforeToolCallbackStructured {
	return func(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
		if args == nil {
			return nil, nil
		}

		// 无身份 → 拒绝。不知道是谁就不放行。
		principal, ok := PrincipalFrom(ctx)
		if !ok {
			p.log("denied: no principal in context (tool=%s)", args.ToolName)
			return denyResult("无法确认你的身份，已拒绝该操作。"), nil
		}

		if p.source == nil {
			p.log("denied: no permission source configured (principal=%s tool=%s)",
				principal.Redacted(), args.ToolName)
			return denyResult("权限未配置，已拒绝该操作。"), nil
		}

		allowed, err := p.source.Allowed(ctx, AccessRequest{
			Principal: principal,
			Action:    args.ToolName,
			// Resource 来自 ctx（由管道注入工作区 ID）。
			// v1 不参与判定，但透传——v2 资源级直接可用，无需再改消费方。
			Resource: ResourceFrom(ctx),
		})
		if err != nil {
			// 查不了 ≠ 不允许。两者都拒，但日志必须区分——
			// 否则数据源故障会被误读为权限收紧。
			p.log("denied: permission source unavailable (principal=%s tool=%s resource=%q err=%v)",
				principal.Redacted(), args.ToolName, ResourceFrom(ctx), err)
			return denyResult("权限校验暂时不可用，请稍后重试。"), nil
		}
		if !allowed {
			// 日志含 principal + action + resource（审计三要素：谁/做什么/对什么）。
			// 角色链路（user→role→permission）在 v2 追加——需 RBAC 侧暴露
			// 判定依据，当前接口契约是纯判定（见 07 文档 §9 AC-5）。
			p.log("denied: not permitted (principal=%s tool=%s resource=%q)",
				principal.Redacted(), args.ToolName, ResourceFrom(ctx))
			return denyResult(denyMessage(args.ToolName)), nil
		}

		p.log("allowed: principal=%s tool=%s resource=%q",
			principal.Redacted(), args.ToolName, ResourceFrom(ctx))
		return nil, nil // 放行
	}
}

func (p *PrincipalPolicyPlugin) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// denyResult 构造拒绝结果。
//
// 文案会进最终回复给用户看，故用可读中文而非内部错误串——
// 与 approval 插件的 "tool X is denied by approval policy" 形成对比：
// 那条是给开发者看的，这条是给用户看的。
func denyResult(msg string) *tool.BeforeToolResult {
	return &tool.BeforeToolResult{CustomResult: msg}
}

// denyMessage 生成权限拒绝的用户可见文案。
//
// **为什么要有「不要重试」这句**（实测缺陷）：首版文案只说「你没有权限」，
// 模型读到后连试 3 次同一个工具——用户日志里出现 3 条
// `[perm] denied: not permitted`，最终回复也是三次失败后的道歉。
//
// 根因：模型把 CustomResult 当普通工具返回值，认为「换个说法再试一次」
// 可能成功。但这是**确定性拒绝**——权限表不会因为重试而改变。
// 明确告知「不要重试」能让模型立即转向回答用户（如「请让管理员授权」）。
//
// 代价（明示）：这句话对模型是行为指令，无法在协议层强制。若模型
// 仍重试，还有一层兜底——chat 层的工具调用轮次上限
// （Options.MaxToolIterations，默认 8；见 chat/execute.go 的 newAgent）。
func denyMessage(toolName string) string {
	return fmt.Sprintf(
		"你没有使用该工具（%s）的权限。这是确定性拒绝，**重试不会成功**，"+
			"请不要再次调用该工具，改为向用户说明需要管理员授权。",
		toolName)
}
