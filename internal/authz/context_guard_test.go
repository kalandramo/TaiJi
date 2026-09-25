package authz

import (
	"context"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// 上下文级守卫的单元测试（issue #6 缺口 1 的接线）。
//
// 不变量：**写操作只允许在可写上下文（interactive）中执行**。
// 只读操作不受上下文限制。缺失 ContextKind 时 fail-closed（写操作拒绝）。

// metaTool 是最小 tool.Tool + MetadataProvider 实现，用于构造 metadata 表。
// （命名避开 toolpolicy_test.go 里已有的 fakeTool。）
type metaTool struct {
	name string
	md   tool.ToolMetadata
}

func (f metaTool) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: f.name}
}

func (f metaTool) ToolMetadata() tool.ToolMetadata { return f.md }

// buildGuard 用工具集构造守卫。
func buildGuard(t *testing.T, tools ...tool.Tool) *ContextGuardPlugin {
	t.Helper()
	return NewContextGuardPlugin(tools, nil)
}

func TestContextGuard_ChannelWriteDenied(t *testing.T) {
	// 渠道上下文 + 写工具 → 拒绝。
	g := buildGuard(t, metaTool{name: "write_note", md: tool.ToolMetadata{ReadOnly: false}})
	ctx := WithContextKind(context.Background(), KindChannel)

	res, err := runGuard(t, g, ctx, "write_note")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.CustomResult == nil {
		t.Fatal("渠道上下文的写操作应被拒绝（CustomResult 非 nil），实际放行")
	}
}

func TestContextGuard_ChannelReadOnlyAllowed(t *testing.T) {
	// 渠道上下文 + 只读工具 → 放行。
	g := buildGuard(t, metaTool{name: "echo", md: tool.ToolMetadata{ReadOnly: true}})
	ctx := WithContextKind(context.Background(), KindChannel)

	res, err := runGuard(t, g, ctx, "echo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil && res.CustomResult != nil {
		t.Fatalf("渠道上下文的只读操作应放行，实际被拒：%v", res.CustomResult)
	}
}

func TestContextGuard_InteractiveWriteAllowed(t *testing.T) {
	// 可写上下文 + 写工具 → 放行。
	g := buildGuard(t, metaTool{name: "write_note", md: tool.ToolMetadata{ReadOnly: false}})
	ctx := WithContextKind(context.Background(), KindInteractive)

	res, err := runGuard(t, g, ctx, "write_note")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil && res.CustomResult != nil {
		t.Fatalf("可写上下文的写操作应放行，实际被拒：%v", res.CustomResult)
	}
}

func TestContextGuard_MissingKindWriteDenied(t *testing.T) {
	// 缺 ContextKind + 写工具 → fail-closed 拒绝。
	g := buildGuard(t, metaTool{name: "write_note", md: tool.ToolMetadata{ReadOnly: false}})

	res, err := runGuard(t, g, context.Background(), "write_note")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.CustomResult == nil {
		t.Fatal("缺失 ContextKind 的写操作应 fail-closed 拒绝，实际放行")
	}
}

func TestContextGuard_MissingKindReadOnlyAllowed(t *testing.T) {
	// 缺 ContextKind + 只读工具 → 放行（只读不需要上下文许可）。
	g := buildGuard(t, metaTool{name: "echo", md: tool.ToolMetadata{ReadOnly: true}})

	res, err := runGuard(t, g, context.Background(), "echo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil && res.CustomResult != nil {
		t.Fatalf("只读操作不应受上下文限制，实际被拒：%v", res.CustomResult)
	}
}

func TestContextGuard_UnknownToolTreatedAsWrite(t *testing.T) {
	// 表里没有的工具 → 视为写（fail-closed）。新增工具不会因缺注解而敞开。
	g := buildGuard(t, metaTool{name: "echo", md: tool.ToolMetadata{ReadOnly: true}})
	ctx := WithContextKind(context.Background(), KindChannel)

	res, err := runGuard(t, g, ctx, "some_unknown_tool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.CustomResult == nil {
		t.Fatal("未在表中的工具应视为写操作并拒绝，实际放行")
	}
}

func TestContextGuard_NilArgs(t *testing.T) {
	// nil args 不应 panic，也不应拦截。
	g := buildGuard(t, metaTool{name: "echo", md: tool.ToolMetadata{ReadOnly: true}})
	cb := g.beforeTool()
	res, err := cb(context.Background(), nil)
	if err != nil || res != nil {
		t.Fatalf("nil args 应放行且无错误，got res=%v err=%v", res, err)
	}
}

// runGuard 调用守卫的 beforeTool 回调。
func runGuard(t *testing.T, g *ContextGuardPlugin, ctx context.Context, toolName string) (*tool.BeforeToolResult, error) {
	t.Helper()
	cb := g.beforeTool()
	return cb(ctx, &tool.BeforeToolArgs{ToolName: toolName})
}
