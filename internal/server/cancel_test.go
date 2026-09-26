package server

import (
	"context"
	"sync"
	"testing"
	"time"
)

// CancelRegistry 测试（SPEC §5.4）。

func TestCancelRegistry_RegisterAndCancel(t *testing.T) {
	r := NewCancelRegistry()
	ctx, cleanup := r.Register(context.Background(), "sess-1")
	defer cleanup()

	// 注册后 ctx 应未取消。
	select {
	case <-ctx.Done():
		t.Fatal("刚注册的 ctx 不应已取消")
	default:
	}

	// Cancel 应真正取消 ctx。
	if !r.Cancel("sess-1") {
		t.Fatal("Cancel 应返回 true（有进行中的 run）")
	}
	select {
	case <-ctx.Done():
		t.Log("✓ Cancel 真正取消了 ctx")
	case <-time.After(time.Second):
		t.Fatal("Cancel 后 ctx 未取消")
	}
}

// SPEC §5.4 边界：无进行中的 run 时 Cancel 返回 false。
func TestCancelRegistry_CancelUnknownSession(t *testing.T) {
	r := NewCancelRegistry()
	if r.Cancel("never-registered") {
		t.Error("未注册的 session Cancel 应返回 false")
	}
	t.Log("✓ 未注册 session 的 Cancel 返回 false（/stop 据此回复「无进行中的生成」）")
}

// cleanup 必须从注册表删除条目——否则泄漏。
func TestCancelRegistry_CleanupRemovesEntry(t *testing.T) {
	r := NewCancelRegistry()
	_, cleanup := r.Register(context.Background(), "sess-1")
	if r.Pending() != 1 {
		t.Fatalf("注册后 Pending = %d, want 1", r.Pending())
	}
	cleanup()
	if r.Pending() != 0 {
		t.Errorf("cleanup 后 Pending = %d, want 0（条目泄漏）", r.Pending())
	}
	t.Log("✓ cleanup 删除条目（无泄漏）")
}

// cleanup 幂等：重复调用安全（用户可能连发两次 /stop）。
func TestCancelRegistry_CleanupIsIdempotent(t *testing.T) {
	r := NewCancelRegistry()
	_, cleanup := r.Register(context.Background(), "sess-1")

	cleanup()
	cleanup() // 第二次不应 panic
	cleanup() // 第三次

	if r.Pending() != 0 {
		t.Errorf("Pending = %d, want 0", r.Pending())
	}
	t.Log("✓ cleanup 幂等（重复调用安全）")
}

// 重新注册同一 session：旧条目被取消，新条目生效。
//
// 这是「保守选择」的行为——宁可取消一个不该取消的 run，
// 也不留下两个竞争同一 session 的 run。
func TestCancelRegistry_ReregisterCancelsPrevious(t *testing.T) {
	r := NewCancelRegistry()

	oldCtx, oldCleanup := r.Register(context.Background(), "sess-1")
	defer oldCleanup()

	newCtx, newCleanup := r.Register(context.Background(), "sess-1")
	defer newCleanup()

	// 旧的应已被取消。
	select {
	case <-oldCtx.Done():
		t.Log("✓ 重新注册取消了旧 ctx")
	case <-time.After(time.Second):
		t.Error("重新注册应取消旧的 ctx")
	}

	// 新的应未取消。
	select {
	case <-newCtx.Done():
		t.Error("新的 ctx 不应被取消")
	default:
	}

	// 旧 cleanup 不应删掉新条目（代际号保护）。
	oldCleanup()
	if r.Pending() != 1 {
		t.Errorf("旧 cleanup 后 Pending = %d, want 1（不应误删新条目）", r.Pending())
	}
	t.Log("✓ 旧 cleanup 不误删新条目（代际号保护）")
}

// 并发安全：多 goroutine 同时 Register/Cancel 不应 panic 或死锁。
func TestCancelRegistry_ConcurrentAccess(t *testing.T) {
	r := NewCancelRegistry()
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sid := "sess-concurrent"
			_, cleanup := r.Register(context.Background(), sid)
			r.Cancel(sid)
			cleanup()
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		t.Logf("✓ 20 并发 Register/Cancel 完成，无死锁（Pending=%d）", r.Pending())
	case <-time.After(5 * time.Second):
		t.Fatal("并发访问死锁")
	}
}

// 父 ctx 取消应传播到子 ctx（run 的 ctx 继承链）。
func TestCancelRegistry_ParentCancellationPropagates(t *testing.T) {
	r := NewCancelRegistry()
	parent, parentCancel := context.WithCancel(context.Background())

	ctx, cleanup := r.Register(parent, "sess-1")
	defer cleanup()

	parentCancel()

	select {
	case <-ctx.Done():
		t.Log("✓ 父 ctx 取消传播到子 ctx")
	case <-time.After(time.Second):
		t.Error("父 ctx 取消应传播")
	}
}
