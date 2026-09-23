// MCP 工具集装配（issue #3）。
//
// 复用 trpc-agent-go 的 tool/mcp，支持 stdio / sse / streamable 三种 transport。
// 本层职责是「把配置变成可用的 ToolSet」，并在上游错误之上补足排障所需的上下文。
package bootstrap

import (
	"context"
	"fmt"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/mcp"
)

// MCPServerConfig 描述一个 MCP server 的连接配置。
type MCPServerConfig struct {
	// Name 是 server 标识，用作工具名前缀（{Name}_{remoteTool}）。必填且唯一。
	// 依据设计文档 §4.2：多 server 时必须用不同 name，否则前缀冲突。
	Name string

	// Transport 是传输方式："stdio" | "sse" | "streamable"（或 streamable_http）。
	Transport string

	// stdio 模式
	Command string
	Args    []string

	// sse / streamable 模式
	URL     string
	Headers map[string]string

	// Timeout 是连接/调用超时，0 表示用上游默认。
	Timeout time.Duration

	// AllowTools 是工具白名单。非空时只有列出的工具会暴露给模型。
	AllowTools []string
}

// NewMCPSets 按配置装配 MCP ToolSet 列表。
//
// 行为：
//   - 配置非法（缺 name/transport/command/url、name 重复）→ 返回错误，不静默跳过
//   - 每个 server 装配后立即 Init 预热 → 失败即返回错误（fail-fast，AC-3）
//   - 错误信息包含 server 名与命令路径 → 路径问题可定位（AC-4）
//
// 返回的 ToolSet 需由调用方负责 Close（装配失败时本函数已回收已建连接）。
func NewMCPSets(cfgs []MCPServerConfig) ([]tool.ToolSet, error) {
	if len(cfgs) == 0 {
		return nil, nil // 无 MCP 是合法配置
	}

	if err := validateMCPServerConfigs(cfgs); err != nil {
		return nil, err
	}

	sets := make([]tool.ToolSet, 0, len(cfgs))
	ctx := context.Background()

	for _, c := range cfgs {
		ts, err := buildOneMCPSet(ctx, c)
		if err != nil {
			closeAll(sets) // 回收已建连接，避免泄漏
			return nil, err
		}
		sets = append(sets, ts)
	}
	return sets, nil
}

// defaultMCPTimeout 是未配置 Timeout 时使用的连接/调用超时。
// 上游 trpc-mcp-go 要求 timeout 必须为正数，传 0 会报
// "invalid configuration: timeout must be positive"。
const defaultMCPTimeout = 30 * time.Second

// buildOneMCPSet 装配并预热单个 server。
func buildOneMCPSet(ctx context.Context, c MCPServerConfig) (tool.ToolSet, error) {
	opts := []mcp.ToolSetOption{
		// 工具名前缀的来源：WithName 让模型看到 {name}_{remoteTool}
		mcp.WithName(c.Name),
	}
	if len(c.AllowTools) > 0 {
		opts = append(opts, mcp.WithToolFilterFunc(tool.NewIncludeToolNamesFilter(c.AllowTools...)))
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultMCPTimeout
	}

	ts := mcp.NewMCPToolSet(mcp.ConnectionConfig{
		Transport: c.Transport,
		ServerURL: c.URL,
		Headers:   c.Headers,
		Command:   c.Command,
		Args:      c.Args,
		Timeout:   timeout,
	}, opts...)

	if err := ts.Init(ctx); err != nil {
		// AC-4：上游错误只含 server name（"failed to initialize MCP tool set %q"），
		// 不含命令路径。路径配错时排障需要知道「想启动的是什么」，
		// 故在此补上 endpoint 描述。
		return nil, fmt.Errorf("mcp server %q (%s): %w",
			c.Name, c.endpointDescription(), err)
	}
	return ts, nil
}

// endpointDescription 描述连接目标，用于错误信息。
func (c MCPServerConfig) endpointDescription() string {
	switch c.Transport {
	case "stdio":
		parts := append([]string{c.Command}, c.Args...)
		return "stdio command=" + strings.Join(parts, " ")
	default:
		return c.Transport + " url=" + c.URL
	}
}

// validateMCPServerConfigs 校验配置，收集全部问题后一次性报告。
func validateMCPServerConfigs(cfgs []MCPServerConfig) error {
	seen := make(map[string]bool, len(cfgs))
	var problems []string

	for i, c := range cfgs {
		if strings.TrimSpace(c.Name) == "" {
			problems = append(problems, fmt.Sprintf("server[%d]: name is required", i))
			continue
		}
		if seen[c.Name] {
			// 同名会导致工具名前缀冲突（AC-2 的反面）
			problems = append(problems, fmt.Sprintf("server[%d]: duplicate name %q", i, c.Name))
		}
		seen[c.Name] = true

		if strings.TrimSpace(c.Transport) == "" {
			problems = append(problems, fmt.Sprintf("server %q: transport is required", c.Name))
			continue
		}

		switch c.Transport {
		case "stdio":
			if strings.TrimSpace(c.Command) == "" {
				problems = append(problems, fmt.Sprintf("server %q: command is required for stdio", c.Name))
			}
		case "sse", "streamable", "streamable_http":
			if strings.TrimSpace(c.URL) == "" {
				problems = append(problems, fmt.Sprintf("server %q: url is required for %s", c.Name, c.Transport))
			}
		default:
			problems = append(problems, fmt.Sprintf(
				"server %q: unsupported transport %q (supported: stdio, sse, streamable)", c.Name, c.Transport))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid MCP config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// closeAll 回收已装配的 ToolSet（装配中途失败时用）。
func closeAll(sets []tool.ToolSet) {
	for _, ts := range sets {
		if closer, ok := ts.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
}

// CloseMCPSets 由调用方在退出时调用，释放全部 MCP 连接。
func CloseMCPSets(sets []tool.ToolSet) {
	closeAll(sets)
}
