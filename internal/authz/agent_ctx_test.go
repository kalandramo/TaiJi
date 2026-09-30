package authz

import (
	"context"
	"testing"
)

// agent 上下文的注入与读取（N5 权限隔离的传输层）。

func TestWithAgent_RoundTrip(t *testing.T) {
	ctx := WithAgent(context.Background(), "ops")
	if got := AgentFrom(ctx); got != "ops" {
		t.Errorf("AgentFrom = %q, want ops", got)
	}
}

// 空 agent 是 no-op（单 agent 部署时不改变 ctx）。
func TestWithAgent_EmptyIsNoop(t *testing.T) {
	base := context.Background()
	ctx := WithAgent(base, "")
	if ctx != base {
		t.Error("空 agent 应是 no-op（不产生新 ctx）")
	}
	if got := AgentFrom(ctx); got != "" {
		t.Errorf("未注入时 AgentFrom 应为空串，got %q", got)
	}
}

// nil ctx 不得 panic。
func TestWithAgent_NilContext(t *testing.T) {
	if ctx := WithAgent(nil, "x"); ctx != nil {
		t.Error("nil ctx 应原样返回")
	}
	if got := AgentFrom(nil); got != "" {
		t.Errorf("nil ctx 应返回空串，got %q", got)
	}
}

// 与 Resource 正交：两者可同时存在且互不干扰。
func TestWithAgent_CoexistsWithResource(t *testing.T) {
	ctx := WithResource(context.Background(), "ws1")
	ctx = WithAgent(ctx, "billing")

	if got := ResourceFrom(ctx); got != "ws1" {
		t.Errorf("ResourceFrom = %q（注入 agent 不应影响 resource）", got)
	}
	if got := AgentFrom(ctx); got != "billing" {
		t.Errorf("AgentFrom = %q", got)
	}
}
