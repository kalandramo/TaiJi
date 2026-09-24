package channel

import (
	"sync"
	"testing"
	"time"
)

// 去重测试（issue #9 Wave 4）。
//
// 背景：飞书事件投递是至少一次语义，响应慢会触发重投。没有去重，
// 同一消息会被处理两次——用户收到两条回答。

func TestDeduper_FirstSeenThenDuplicate(t *testing.T) {
	d := NewDeduper(time.Minute)

	if d.Seen("om_1") {
		t.Error("first sighting must not be a duplicate")
	}
	if !d.Seen("om_1") {
		t.Error("second sighting must be a duplicate")
	}
	if !d.Seen("om_1") {
		t.Error("third sighting must also be a duplicate")
	}
}

func TestDeduper_DistinctIDsAreIndependent(t *testing.T) {
	d := NewDeduper(time.Minute)

	if d.Seen("om_1") {
		t.Error("om_1 first sighting must not be duplicate")
	}
	// 不同 ID 必须各自独立——否则一条消息会「吃掉」另一条。
	if d.Seen("om_2") {
		t.Error("om_2 first sighting must not be duplicate")
	}
	if !d.Seen("om_1") || !d.Seen("om_2") {
		t.Error("both must now be duplicates")
	}
}

func TestDeduper_TTLExpiryAllowsReprocess(t *testing.T) {
	// 过期后同一 ID 可再次被处理——否则长期运行会永久记住所有消息，
	// 内存无界增长，且平台若复用 ID（理论上不会，但防御）会被误丢。
	d := NewDeduper(time.Minute)

	base := time.Now()
	d.now = func() time.Time { return base }

	if d.Seen("om_1") {
		t.Fatal("first sighting must not be duplicate")
	}

	// 推进到 TTL 之后
	d.now = func() time.Time { return base.Add(2 * time.Minute) }

	if d.Seen("om_1") {
		t.Error("after TTL expiry the id must be treatable as new")
	}
}

func TestDeduper_EvictsExpiredEntries(t *testing.T) {
	// 过期条目必须被清理，否则内存无界增长。
	d := NewDeduper(time.Minute)

	base := time.Now()
	d.now = func() time.Time { return base }

	for _, id := range []string{"a", "b", "c"} {
		d.Seen(id)
	}
	if n := d.Len(); n != 3 {
		t.Fatalf("Len = %d, want 3", n)
	}

	// 推进时间后写入新 ID，触发清理
	d.now = func() time.Time { return base.Add(2 * time.Minute) }
	d.Seen("d")

	if n := d.Len(); n != 1 {
		t.Errorf("Len = %d, want 1 (expired entries must be evicted)", n)
	}
}

func TestDeduper_EmptyIDNeverDeduplicated(t *testing.T) {
	// 空 ID 无法作为去重键。把它当「已见」会让所有缺 ID 的消息被静默丢弃
	// ——那是更糟的失效方向（丢消息 > 重复处理）。
	d := NewDeduper(time.Minute)

	if d.Seen("") {
		t.Error("empty id must never be reported as duplicate")
	}
	if d.Seen("") {
		t.Error("empty id must never be reported as duplicate (repeat)")
	}
}

func TestDeduper_ConcurrentSeenOnlyOneWins(t *testing.T) {
	// webhook 的多个请求可能同时到达同一消息（平台并发重投）。
	// 必须恰好一个「首次」胜出，否则仍会重复处理。
	d := NewDeduper(time.Minute)
	const n = 32

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	firsts := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if !d.Seen("om_same") {
				mu.Lock()
				firsts++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if firsts != 1 {
		t.Errorf("first sightings = %d, want exactly 1 (concurrent duplicates must be collapsed)", firsts)
	}
}

func TestDeduper_DefaultTTLWhenNonPositive(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		d := NewDeduper(ttl)
		if d.ttl != DefaultDedupTTL {
			t.Errorf("ttl = %v, want default %v", d.ttl, DefaultDedupTTL)
		}
	}
}
