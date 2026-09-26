package authz

import (
	"context"
	"errors"
	"fmt"
)

// 上下文级降权（issue #6，设计文档 §4.3.3）。
//
// 动机：IM 渠道只验签不验用户身份，无法可靠判定个人角色。故渠道来源的
// 执行上下文一律降为只读——写操作被拒。
//
// 与工具级策略（toolpolicy.go）的分工：工具级管"哪个工具能跑"，
// 上下文级管"这次执行能做什么"。两者正交，都需要。

// ContextKind 标识一次执行的来源，是权限判定的依据。
type ContextKind string

const (
	// KindInteractive 顶层交互式会话——唯一可写的上下文（按 ACL 细分）。
	KindInteractive ContextKind = "interactive"
	// KindScheduled 定时任务触发——只读。
	KindScheduled ContextKind = "scheduled"
	// KindSubagent 子 agent 委派——只读。
	KindSubagent ContextKind = "subagent"
	// KindChannel IM 渠道消息——只读。
	KindChannel ContextKind = "channel"
)

var (
	// ErrContextKindMissing 表示上下文未携带 ContextKind。
	// 这是 fail-closed 的触发条件：不默认放行。
	ErrContextKindMissing = errors.New("authz: context kind missing")
	// ErrWriteDenied 表示当前上下文不允许写操作。
	ErrWriteDenied = errors.New("authz: write denied")
)

type ctxKey struct{}

// WithContextKind 注入执行上下文类型。
func WithContextKind(ctx context.Context, k ContextKind) context.Context {
	return context.WithValue(ctx, ctxKey{}, k)
}

// ContextKindFrom 读取上下文类型。第二个返回值为 false 表示未设置。
//
// 调用方应把 false 当作 fail-closed 信号，而不是"当作 interactive"。
func ContextKindFrom(ctx context.Context) (ContextKind, bool) {
	if ctx == nil {
		return "", false
	}
	k, ok := ctx.Value(ctxKey{}).(ContextKind)
	return k, ok
}

// RequireWritable 在写操作前校验。允许返回 nil，拒绝返回 error。
//
// fail-closed 的两条路径：
//   - 上下文缺失 ContextKind → ErrContextKindMissing（不默认为 interactive）
//   - kind 不是 KindInteractive → ErrWriteDenied（未知 kind 也拒，只放行 interactive）
func RequireWritable(ctx context.Context) error {
	k, ok := ContextKindFrom(ctx)
	if !ok {
		return ErrContextKindMissing
	}
	if k != KindInteractive {
		return fmt.Errorf("%w: context %q is read-only", ErrWriteDenied, k)
	}
	return nil
}

// ── 资源上下文（issue #6 决策二：resource 注入）──
//
// 动机：权限查询是 (谁, 做什么, 对什么) 三元组。前两者已在 ctx 里
// （Principal / ContextKind），资源标识此前无处承载。
//
// 来源：IM 渠道的资源天然是**当前工作区**——管道在路由后即可拿到
// （server/pipeline.go 的 p.route.WorkspaceID）。
//
// v1 不参与判定（权限表只看工具名），但**透传**给 AccessRequest——
// 这样 v2 资源级判定无需再改消费方（permission_plugin.go）。
//
// 与 Principal 的分工：Principal 管「谁」，Resource 管「对什么」，
// ContextKind 管「来源是否允许写」。三者正交，可同时存在。

type resourceCtxKey struct{}

// WithResource 把资源标识注入上下文。空值等价于未注入。
func WithResource(ctx context.Context, resource string) context.Context {
	if ctx == nil || resource == "" {
		return ctx
	}
	return context.WithValue(ctx, resourceCtxKey{}, resource)
}

// ResourceFrom 读取资源标识。无资源时返回空串。
//
// 空串是合法状态（如 CLI 无工作区概念）——调用方据此按「无资源」处理，
// 不必 fail-closed（资源级判定在 v2，v1 不依赖它）。
func ResourceFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(resourceCtxKey{}).(string)
	return v
}
