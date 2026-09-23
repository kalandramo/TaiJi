package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 上下文级降权契约（issue #6 AC-5/AC-6，设计文档 §4.3.3）：
//   - 放行路径注入的上下文必须携带 ContextKind=channel
//   - 在该上下文下调用写操作 → 拒绝
//   - 上下文缺失 ContextKind 时写操作被拒（不得默认为 interactive）

func TestWithContextKind_RoundTrip(t *testing.T) {
	ctx := WithContextKind(context.Background(), KindChannel)
	got, ok := ContextKindFrom(ctx)
	if !ok {
		t.Fatal("ContextKindFrom returned ok=false after WithContextKind")
	}
	if got != KindChannel {
		t.Errorf("kind = %q, want %q", got, KindChannel)
	}
}

func TestRequireWritable_MissingKindFailsClosed(t *testing.T) {
	// AC-6：上下文缺失时必须拒绝，不得默认为 interactive。
	err := RequireWritable(context.Background())
	if err == nil {
		t.Fatal("RequireWritable with no ContextKind returned nil, want error (fail-closed)")
	}
	if !errors.Is(err, ErrContextKindMissing) {
		t.Errorf("err = %v, want ErrContextKindMissing", err)
	}
}

func TestRequireWritable_ChannelIsReadOnly(t *testing.T) {
	// AC-5：IM 渠道来源 → 写操作被拒。
	ctx := WithContextKind(context.Background(), KindChannel)
	err := RequireWritable(ctx)
	if err == nil {
		t.Fatal("RequireWritable in channel context returned nil, want error")
	}
	if !errors.Is(err, ErrWriteDenied) {
		t.Errorf("err = %v, want ErrWriteDenied", err)
	}
	if !strings.Contains(err.Error(), string(KindChannel)) {
		t.Errorf("err %q should name the offending context kind", err.Error())
	}
}

func TestRequireWritable_InteractiveAllowed(t *testing.T) {
	ctx := WithContextKind(context.Background(), KindInteractive)
	if err := RequireWritable(ctx); err != nil {
		t.Errorf("interactive context should be writable, got %v", err)
	}
}

func TestRequireWritable_OtherReadOnlyKinds(t *testing.T) {
	// scheduled / subagent 同样是只读（设计文档 §4.3.3 的 ContextKind 表）。
	for _, k := range []ContextKind{KindScheduled, KindSubagent} {
		ctx := WithContextKind(context.Background(), k)
		if err := RequireWritable(ctx); err == nil {
			t.Errorf("kind %q should be read-only, got nil error", k)
		}
	}
}

func TestRequireWritable_UnknownKindIsRejected(t *testing.T) {
	// 未知 kind（未来新增或拼写错误）不得被当作可写——只放行 interactive。
	ctx := WithContextKind(context.Background(), ContextKind("some-future-kind"))
	if err := RequireWritable(ctx); err == nil {
		t.Error("unknown ContextKind should not be treated as writable")
	}
}

func TestContextKindFrom_NilContext(t *testing.T) {
	// nil ctx 不应 panic（防御性：调用方可能传 nil）。
	//nolint:staticcheck // 有意测试 nil 输入
	_, ok := ContextKindFrom(nil)
	if ok {
		t.Error("ContextKindFrom(nil) returned ok=true, want false")
	}
}
