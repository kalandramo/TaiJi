package authz

import (
	"context"
	"testing"
)

// 身份上下文（issue #10：飞书用户身份贯通）。
//
// 动机：open_id 目前只到门禁为止（pipeline.go:130 用它做 owner 判定），
// 执行层拿不到提问人是谁——工具策略无法按用户区分。
//
// 安全不变量：**不构造匿名主体**。无身份时 PrincipalFrom 返回 ok=false，
// 调用方必须把它当 fail-closed 信号，而非"当作放行"。
// 这与 ResolvePrincipal 的设计一致（空 OpenID → 返回空 ID 而非匿名主体）。

func TestWithPrincipal_RoundTrip(t *testing.T) {
	p := Principal{Type: "im_user", ID: "feishu:feishu:ou_alice"}
	ctx := WithPrincipal(context.Background(), p)

	got, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("PrincipalFrom 应返回 ok=true")
	}
	if got != p {
		t.Errorf("Principal = %+v, want %+v", got, p)
	}
}

// 未注入身份时 ok=false —— 调用方据此 fail-closed。
func TestPrincipalFrom_AbsentIsNotOK(t *testing.T) {
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Error("未注入时应返回 ok=false")
	}
}

// nil ctx 不 panic（与 ContextKindFrom 的处理一致）。
func TestPrincipalFrom_NilContext(t *testing.T) {
	if _, ok := PrincipalFrom(nil); ok {
		t.Error("nil ctx 应返回 ok=false")
	}
}

// 空 ID 的主体视为"无身份"——防止构造出无法比对的主体。
func TestWithPrincipal_EmptyIDIsNotOK(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Type: "im_user", ID: ""})
	if _, ok := PrincipalFrom(ctx); ok {
		t.Error("空 ID 的主体应被当作无身份（ok=false）")
	}
}

// 身份与 ContextKind 互不干扰（两者正交）。
func TestPrincipal_IndependentFromContextKind(t *testing.T) {
	ctx := WithContextKind(context.Background(), KindChannel)
	ctx = WithPrincipal(ctx, Principal{Type: "im_user", ID: "feishu:feishu:ou_bob"})

	if k, ok := ContextKindFrom(ctx); !ok || k != KindChannel {
		t.Errorf("ContextKind = %v/%v, want KindChannel/true", k, ok)
	}
	if p, ok := PrincipalFrom(ctx); !ok || p.ID != "feishu:feishu:ou_bob" {
		t.Errorf("Principal = %+v/%v, want ou_bob/true", p, ok)
	}
}

// OpenID 缺失时 ResolvePrincipal 产出空主体，PrincipalFrom 应视为无身份。
//
// 这条锁住整条链路的 fail-closed 语义：平台没给 open_id 时，
// 不能凭空造一个"匿名用户"让权限判定失去依据。
func TestResolvePrincipal_ThenContext_EmptyOpenIDStaysInvalid(t *testing.T) {
	p := ResolvePrincipal(PrincipalInput{ChannelID: "c1", Platform: "feishu", OpenID: ""})
	if p.Valid() {
		t.Error("空 OpenID 解析出的主体不应 Valid")
	}
	ctx := WithPrincipal(context.Background(), p)
	if _, ok := PrincipalFrom(ctx); ok {
		t.Error("空主体注入后 PrincipalFrom 应返回 ok=false")
	}
}
