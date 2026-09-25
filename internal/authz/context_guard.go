package authz

import (
	"context"
	"fmt"

	"trpc.group/trpc-go/trpc-agent-go/plugin"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// 上下文级守卫：把「IM 来源只读」（设计文档 §4.3.3 / FR-10.7）从**声明**变为**执行**。
//
// 背景（issue #6 缺口）：context.go 的 RequireWritable 定义了写操作校验，
// 但**无任何生产代码调用它**——"IM 来源只读"此前只是声明。本插件是它的执行点。
//
// 为什么用独立插件而非改造 approval：approval 只按工具名判定、不感知上下文
// （approval.go 的静态映射），而本判定依据是「来源 + 工具是否只读」两个维度。
//
// 为什么不用 trpc 的 PermissionPolicy：那条路径需要从 RunOptions 注入
// （functioncall.go:3654 的 invocation.RunOptions.ToolPermissionPolicy），
// 与既有 plugin.BeforeTool 机制并列会引入**第二条权限机制**，让权限散在两处。
// 本插件复用既有插件链，与 PrincipalPolicyPlugin 同层。
type ContextGuardPlugin struct {
	// readOnly 是工具名 → 是否只读。判定依据来自 tool.MetadataOf
	// （MCP 的 readOnlyHint 注解经 mcpTool.ToolMetadata 透传，已实测）。
	//
	// 表里没有的工具视为**写操作**（fail-closed）——新增工具不会因
	// 缺注解而敞开。
	readOnly map[string]bool
	// logf 记录拒绝原因，可为 nil。拒绝必须留痕。
	logf func(format string, args ...any)
}

// NewContextGuardPlugin 用工具集构造守卫。
//
// tools 为 nil 时 readOnly 表为空 → 所有工具视为写操作（fail-closed）。
func NewContextGuardPlugin(tools []tool.Tool, logf func(string, ...any)) *ContextGuardPlugin {
	readOnly := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		d := t.Declaration()
		if d == nil || d.Name == "" {
			continue
		}
		readOnly[d.Name] = tool.MetadataOf(t).ReadOnly
	}
	return &ContextGuardPlugin{readOnly: readOnly, logf: logf}
}

// Name 实现 plugin.Plugin。
func (p *ContextGuardPlugin) Name() string { return "context_guard" }

// Register 实现 plugin.Plugin。
func (p *ContextGuardPlugin) Register(r *plugin.Registry) {
	if r == nil {
		return
	}
	r.BeforeTool(p.beforeTool())
}

// beforeTool 是判定入口。
//
// 规则（与 RequireWritable 的不变量一致）：
//   - 只读工具 → 任何上下文都放行（读不需要写许可）。
//   - 写工具 → 仅在可写上下文（interactive）放行；其余（含缺失 kind）拒绝。
func (p *ContextGuardPlugin) beforeTool() tool.BeforeToolCallbackStructured {
	return func(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
		if args == nil {
			return nil, nil
		}
		// 只读工具不受上下文限制。
		if p.readOnly[args.ToolName] {
			return nil, nil
		}
		// 写工具：走 RequireWritable 的不变量（缺失 kind → fail-closed）。
		if err := RequireWritable(ctx); err != nil {
			p.log("denied: write tool in non-writable context (tool=%s err=%v)",
				args.ToolName, err)
			return &tool.BeforeToolResult{
				CustomResult: writeDenyMessage(args.ToolName),
			}, nil
		}
		return nil, nil
	}
}

func (p *ContextGuardPlugin) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// writeDenyMessage 生成写操作被拒的用户可见文案。
//
// 与 permission_plugin.go 的 denyMessage 同样的理由：明确「不要重试」，
// 因为这是确定性拒绝——上下文不会因为重试而变成可写。
func writeDenyMessage(toolName string) string {
	return fmt.Sprintf(
		"当前来源不允许执行写操作（工具 %s）。这是确定性拒绝，**重试不会成功**，"+
			"请不要再次调用该工具，改为向用户说明需要到 Web 端完成该操作。",
		toolName)
}
