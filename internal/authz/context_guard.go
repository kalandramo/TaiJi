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

// skillReadOnlyTools 是**上游漏标 ReadOnly 的 skill 读类工具**修正名单。
//
// 为什么需要它（实测确认，2026-09-26）：
// 上游 trpc-agent-go v1.11.2 的 skill 工具**未实现 tool.MetadataProvider**
// （`grep -rln ToolMetadata` 对 `tool/skill/` 返回空），故 `tool.MetadataOf`
// 返回零值 → `ReadOnly=false`。实测 `KindChannel` 下：
//
//	skill_load         被拒=true   ← "write tool in non-writable context"
//	skill_list_docs    被拒=true
//	skill_select_docs  被拒=true
//
// 但三者**事实只读**：
//   - skill_load        把 SKILL.md 正文读进模型上下文
//   - skill_list_docs   列出 skill 的文档
//   - skill_select_docs 只改**会话内**的文档选择状态（读 ctx，无外部副作用）
//
// **为什么不用前缀匹配**：同一家族的 skill_run / skill_exec /
// skill_write_stdin / skill_poll_session / skill_kill_session 是**真写操作**
// （执行代码、写 stdin）。若按 `skill_` 前缀放行，切到
// `SkillToolProfileFull` 时会把这些一并放行——**开的洞比要修的问题更大**。
// 故必须**精确名**逐个列出。
//
// **上游修好后应复查本名单**：若将来 `tool/skill/` 实现了 MetadataProvider
// 并正确标注 ReadOnly，此名单即冗余（保留无害——只在注解缺失时生效，
// 不会覆盖上游的显式声明）。
var skillReadOnlyTools = map[string]struct{}{
	"skill_load":        {},
	"skill_list_docs":   {},
	"skill_select_docs": {},
}

// isSkillReadOnlyOverride 判断工具名是否在 skill 只读修正名单中。
func isSkillReadOnlyOverride(name string) bool {
	_, ok := skillReadOnlyTools[name]
	return ok
}

// NewContextGuardPlugin 用工具集构造守卫。
//
// tools 为 nil 时 readOnly 表为空 → 所有工具视为写操作（fail-closed）。
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
		// 上游注解优先；注解缺失（false）时，skill 读类工具由
		// skillReadOnlyTools 修正为只读（见该变量注释）。
		ro := tool.MetadataOf(t).ReadOnly
		if !ro && isSkillReadOnlyOverride(d.Name) {
			ro = true
		}
		readOnly[d.Name] = ro
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
