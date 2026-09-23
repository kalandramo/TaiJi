package concurrency

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ===== AC-5：派生键是纯字符串操作，无 IO、无 error =====

func TestDeriveKey_StripsSessionSuffix(t *testing.T) {
	// 取 '#' 之前的部分：同 workspace 的多个会话共享串行化域（NFR-8.2）。
	// 匹配依据是派生键而非 JID——否则虚拟 JID 会让本该串行的消息并发起来。
	cases := []struct{ jid, want string }{
		{"feishu:ws1#chatA", "feishu:ws1"},
		{"feishu:ws1#chatB", "feishu:ws1"}, // 与上面同域
		{"feishu:ws1#oc_group#thread:t1#root:r1", "feishu:ws1"},
		{"telegram:ws1#chatA", "telegram:ws1"},
		{"feishu:ws2#chatA", "feishu:ws2"},
	}
	for _, tc := range cases {
		if got := DeriveKey(tc.jid); got != tc.want {
			t.Errorf("DeriveKey(%q) = %q, want %q", tc.jid, got, tc.want)
		}
	}
}

func TestDeriveKey_NoSeparatorReturnsInput(t *testing.T) {
	// 无 '#' 时原样返回（不 panic、不返回空）。
	for _, jid := range []string{"feishu:ws1", "chatA", ""} {
		if got := DeriveKey(jid); got != jid {
			t.Errorf("DeriveKey(%q) = %q, want identity", jid, got)
		}
	}
}

func TestDeriveKey_IsPure(t *testing.T) {
	// 纯函数：同输入恒同输出，且不修改输入（值语义天然保证，此处锁定契约）。
	const jid = "feishu:ws1#chatA"
	first := DeriveKey(jid)
	for i := 0; i < 100; i++ {
		if got := DeriveKey(jid); got != first {
			t.Fatalf("DeriveKey is not pure: %q != %q", got, first)
		}
	}
	if jid != "feishu:ws1#chatA" {
		t.Fatal("DeriveKey must not mutate its input")
	}
}

// ===== AC-1：同一 JID 并发 Acquire 只有一次成功 =====

func TestAcquire_OnlyOneSucceedsPerDomain(t *testing.T) {
	s := NewSerializer()
	const n = 16

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	var releases []func()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 启动栅栏：让 n 个 goroutine 尽量同时冲
			rel, ok := s.Acquire("feishu:ws1#chatA")
			mu.Lock()
			defer mu.Unlock()
			if ok {
				successes++
				releases = append(releases, rel)
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (domain must be exclusive)", successes)
	}
	for _, r := range releases {
		r()
	}
}

func TestAcquire_ReleaseAllowsReacquire(t *testing.T) {
	s := NewSerializer()
	rel, ok := s.Acquire("feishu:ws1#chatA")
	if !ok {
		t.Fatal("first acquire must succeed")
	}
	if _, ok := s.Acquire("feishu:ws1#chatA"); ok {
		t.Fatal("second acquire while held must fail")
	}
	rel()
	if _, ok := s.Acquire("feishu:ws1#chatA"); !ok {
		t.Fatal("acquire after release must succeed")
	}
}

func TestAcquire_ReleaseIsIdempotent(t *testing.T) {
	// release 被调用两次不能破坏状态（不能把别人的占用误删）。
	s := NewSerializer()
	rel, ok := s.Acquire("feishu:ws1#chatA")
	if !ok {
		t.Fatal("acquire must succeed")
	}
	rel()
	rel() // 二次调用不应 panic，也不应解锁未持有的域
	rel2, ok := s.Acquire("feishu:ws1#chatA")
	if !ok {
		t.Fatal("acquire after double release must succeed")
	}
	rel2()
}

// ===== AC-2：同 workspace 不同 JID 共享串行化域 =====

func TestAcquire_SameWorkspaceSharesDomain(t *testing.T) {
	s := NewSerializer()
	relA, okA := s.Acquire("feishu:ws1#chatA")
	if !okA {
		t.Fatal("acquire chatA must succeed")
	}
	defer relA()

	// 同 workspace、不同 JID → 派生键相同 → 必须失败（这是 AC-2 的核心）。
	if _, ok := s.Acquire("feishu:ws1#chatB"); ok {
		t.Error("different JID in same workspace must share the domain (derived key must be used, not the JID)")
	}
	// 话题后缀也要归并到同一域。
	if _, ok := s.Acquire("feishu:ws1#chatA#thread:t1#root:r1"); ok {
		t.Error("thread suffix must fold into the same workspace domain")
	}

	// 对照：不同 workspace → 独立域（否则串行化会退化成全局锁）。
	relC, okC := s.Acquire("feishu:ws2#chatA")
	if !okC {
		t.Error("different workspace must have an independent domain")
	} else {
		relC()
	}
	// 对照：不同渠道 → 独立域。
	relD, okD := s.Acquire("telegram:ws1#chatA")
	if !okD {
		t.Error("different channel must have an independent domain")
	} else {
		relD()
	}
}

// ===== AC-4：release 后等待者能继续（不永久阻塞） =====

func TestAcquireBlocking_WaiterProceedsAfterRelease(t *testing.T) {
	s := NewSerializer()
	rel, ok := s.Acquire("feishu:ws1#chatA")
	if !ok {
		t.Fatal("first acquire must succeed")
	}

	acquired := make(chan struct{})
	go func() {
		rel2, err := s.AcquireBlocking(context.Background(), "feishu:ws1#chatA")
		if err != nil {
			t.Errorf("blocking acquire failed: %v", err)
			close(acquired)
			return
		}
		close(acquired)
		rel2()
	}()

	// 等待者不得在 release 前拿到域。
	select {
	case <-acquired:
		t.Fatal("waiter acquired the domain before release")
	case <-time.After(60 * time.Millisecond):
	}

	rel()

	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not proceed after release (permanent block)")
	}
}

func TestAcquireBlocking_ContextCancel(t *testing.T) {
	// 排队必须可取消，否则关闭流程会挂住。
	s := NewSerializer()
	rel, _ := s.Acquire("feishu:ws1#chatA")
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := s.AcquireBlocking(ctx, "feishu:ws1#chatA")
	if err == nil {
		t.Error("cancelled context must return an error instead of blocking forever")
	}
}

func TestAcquireBlocking_FreeDomainReturnsImmediately(t *testing.T) {
	s := NewSerializer()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rel, err := s.AcquireBlocking(ctx, "feishu:ws1#chatA")
	if err != nil {
		t.Fatalf("free domain must acquire without error, got %v", err)
	}
	rel()
}

// ===== AC-3：并发不交错（事件序列断言） =====

// eventLog 模拟 session log：串行化正确时事件必然成对，交错时会出现
// enter:A enter:B exit:A exit:B 这类序列。
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func TestSerial_NoInterleaving(t *testing.T) {
	// Demo path：并发投两条到同一会话 → 串行处理，log 无交错。
	s := NewSerializer()
	log := &eventLog{}

	run := func(name string) {
		rel, err := s.AcquireBlocking(context.Background(), "feishu:ws1#chatA")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		defer rel()
		log.add("enter:" + name)
		// 临界区持续一段时间——未串行化的实现会在此窗口内观察到交错。
		time.Sleep(40 * time.Millisecond)
		log.add("exit:" + name)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); run("A") }()
	go func() { defer wg.Done(); run("B") }()
	wg.Wait()

	ev := log.snapshot()
	if len(ev) != 4 {
		t.Fatalf("events = %v, want exactly 4", ev)
	}
	// 必须成对出现：enter:X 紧跟 exit:X。
	for i := 0; i < len(ev); i += 2 {
		if !strings.HasPrefix(ev[i], "enter:") || !strings.HasPrefix(ev[i+1], "exit:") {
			t.Fatalf("interleaved session log: %v", ev)
		}
		a := strings.TrimPrefix(ev[i], "enter:")
		b := strings.TrimPrefix(ev[i+1], "exit:")
		if a != b {
			t.Fatalf("enter/exit mismatch: %v", ev)
		}
	}
}

func TestSerial_AtMostOneActiveRun(t *testing.T) {
	// AC-1 的压测版本：任意时刻该域至多一个 active run。
	// 用原子计数在临界区内自检——比事后看事件序列更严格（能捕获瞬间重叠）。
	s := NewSerializer()
	const workers = 32

	var active int32
	var maxSeen int32
	var violations int32
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := s.AcquireBlocking(context.Background(), "feishu:ws1#chatA")
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer rel()

			n := atomic.AddInt32(&active, 1)
			// 记录观察到的最大并发数。
			for {
				old := atomic.LoadInt32(&maxSeen)
				if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
					break
				}
			}
			if n > 1 {
				atomic.AddInt32(&violations, 1)
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&active, -1)
		}()
	}
	wg.Wait()

	if v := atomic.LoadInt32(&violations); v != 0 {
		t.Errorf("observed %d concurrent runs in one domain, want 0", v)
	}
	if m := atomic.LoadInt32(&maxSeen); m != 1 {
		t.Errorf("max concurrent runs = %d, want 1", m)
	}
}

func TestSerial_SameWorkspaceDifferentJIDsDoNotInterleave(t *testing.T) {
	// AC-2 的并发版本：同 workspace 下两个不同 JID 也必须串行（派生键生效）。
	s := NewSerializer()
	log := &eventLog{}

	run := func(name, jid string) {
		rel, err := s.AcquireBlocking(context.Background(), jid)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		defer rel()
		log.add("enter:" + name)
		time.Sleep(40 * time.Millisecond)
		log.add("exit:" + name)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); run("A", "feishu:ws1#chatA") }()
	go func() { defer wg.Done(); run("B", "feishu:ws1#chatB") }()
	wg.Wait()

	ev := log.snapshot()
	if len(ev) != 4 {
		t.Fatalf("events = %v, want 4", ev)
	}
	for i := 0; i < len(ev); i += 2 {
		if !strings.HasPrefix(ev[i], "enter:") || !strings.HasPrefix(ev[i+1], "exit:") {
			t.Fatalf("different JIDs in one workspace interleaved: %v", ev)
		}
	}
}
