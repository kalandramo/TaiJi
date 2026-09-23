// Package authz 承载权限决策（issue #4 起）。
//
// 本文件实现工具级策略：默认拒绝 + 白名单放行。
//
// 安全基线（设计文档 §4.3.2）：默认策略必须是 denied——新增 MCP 工具
// 不会自动获得执行权，必须显式放行。这补上 issue #3 留下的缺口：
// 没有策略层时，任何被列出的工具都能跑。
package authz

import (
	"fmt"
	"sort"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/plugin"
	"trpc.group/trpc-go/trpc-agent-go/plugin/guardrail/approval"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// Policy 是本层对外的策略值（屏蔽上游的三态，只暴露需要的两态）。
type Policy string

const (
	// PolicyAllowed 放行工具执行。
	PolicyAllowed Policy = "allowed"
	// PolicyDenied 拒绝工具执行。拒绝以 CustomResult 形式回给模型，
	// 不中断 run（上游 tool/callbacks.go:77：CustomResult 非 nil 则跳过执行并返回该结果）。
	PolicyDenied Policy = "denied"
)

// ToolPolicyConfig 是策略装配的输入。
type ToolPolicyConfig struct {
	// Allow 是白名单（已放行的工具名）。空表示全部拒绝。
	Allow []string

	// Registered 是当前已注册的工具集合，用于校验白名单条目的存在性。
	// 为空时跳过校验（例：未挂载 MCP server，无工具面可校验）。
	//
	// 注意：这里的名字必须是「模型可见名」（MCP 工具带 {server}_ 前缀，
	// 见 issue #3），因为 approval 插件是按模型传入的 tool name 匹配的。
	Registered []tool.Tool
}

// ToolPolicy 是装配后的策略句柄，包装上游插件并提供查询能力。
type ToolPolicy struct {
	plugin  plugin.Plugin
	allow   map[string]bool
	ordered []string // 稳定顺序的白名单，用于装配与展示
}

// BuildToolPolicy 装配工具策略插件。
//
// 校验（AC-4）：白名单中的名字必须存在于 Registered（若提供了的话）；
// 未注册的名字会被报告，不静默忽略——否则"配了却没生效"极难排查。
func BuildToolPolicy(cfg ToolPolicyConfig) (*ToolPolicy, error) {
	allow := make(map[string]bool, len(cfg.Allow))
	ordered := make([]string, 0, len(cfg.Allow))
	for _, raw := range cfg.Allow {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("authz: blank tool name in allow list")
		}
		if allow[name] {
			continue // 重复项去重，不视为错误
		}
		allow[name] = true
		ordered = append(ordered, name)
	}

	if err := validateAllowList(ordered, cfg.Registered); err != nil {
		return nil, err
	}

	opts := []approval.Option{
		// 安全基线：默认拒绝。未显式放行的工具一律不执行。
		approval.WithDefaultToolPolicy(approval.ToolPolicyDenied),
	}
	for _, name := range ordered {
		opts = append(opts, approval.WithToolPolicy(name, approval.ToolPolicySkipApproval))
	}

	p, err := approval.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("authz: build approval plugin: %w", err)
	}

	sort.Strings(ordered)
	return &ToolPolicy{plugin: p, allow: allow, ordered: ordered}, nil
}

// Plugin 返回可挂到 runner 的插件实例。
func (p *ToolPolicy) Plugin() plugin.Plugin { return p.plugin }

// PolicyFor 返回某工具名的策略。
func (p *ToolPolicy) PolicyFor(name string) Policy {
	if p.allow[name] {
		return PolicyAllowed
	}
	return PolicyDenied
}

// DefaultPolicy 返回默认策略（恒为 denied，是本层的安全不变量）。
func (p *ToolPolicy) DefaultPolicy() Policy { return PolicyDenied }

// AllowedTools 返回排序后的白名单副本。
func (p *ToolPolicy) AllowedTools() []string {
	out := make([]string, len(p.ordered))
	copy(out, p.ordered)
	return out
}

// validateAllowList 校验白名单条目是否都已注册。
//
// Registered 为空时跳过（无工具面可校验，见 ToolPolicyConfig.Registered 说明）。
func validateAllowList(allow []string, registered []tool.Tool) error {
	if len(registered) == 0 {
		return nil
	}

	known := make(map[string]bool, len(registered))
	for _, t := range registered {
		if t == nil {
			continue
		}
		if d := t.Declaration(); d != nil {
			known[d.Name] = true
		}
	}

	var unknown []string
	for _, name := range allow {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		// 同时列出已知名，让排错不必再去翻代码——尤其是 MCP 工具带前缀，
		// 配错前缀（写 echo 而非 srvA_echo）是最常见的失误。
		knownList := make([]string, 0, len(known))
		for k := range known {
			knownList = append(knownList, k)
		}
		sort.Strings(knownList)
		return fmt.Errorf(
			"authz: allow list names %d tool(s) that are not registered: %s (registered: %s)",
			len(unknown), strings.Join(unknown, ", "), strings.Join(knownList, ", "))
	}
	return nil
}

// ToolNames 提取工具集里的模型可见名（供调用方做白名单校验）。
func ToolNames(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		if d := t.Declaration(); d != nil {
			out = append(out, d.Name)
		}
	}
	return out
}
