package channel

import (
	"sync"
	"time"
)

// 消息去重（issue #9 Wave 4）。
//
// 为什么需要：飞书的事件投递是**至少一次**语义。若我们的 HTTP 端点
// 响应慢于平台的超时窗口，平台会重投同一事件；长连接模式在断线重连后
// 也可能补投。没有去重，同一条消息会被处理两次——用户收到两条回答，
// 且第二次的 run 还会带上第一次的历史，产生重复对话。
//
// 为什么不用「处理完再响应」来避免重投：agent 跑数秒，必然超过平台的
// 响应窗口。正确做法是**先立即 200，再异步处理**，并用去重兜住重投。
// 见 cmd/taiji 的 OnMessage 接线。

// DefaultDedupTTL 是消息 ID 的记忆时长。
//
// 取值依据：飞书的事件重投集中在首次投递失败后的数分钟内（指数退避），
// 10 分钟足以覆盖重投窗口，又不会让内存无界增长。
const DefaultDedupTTL = 10 * time.Minute

// Deduper 按消息 ID 做幂等判定。
//
// 并发安全：webhook 的多个请求可能同时到达。
type Deduper struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration

	// now 便于测试注入时间源，避免依赖真实时钟。
	now func() time.Time
}

// NewDeduper 构造去重器。ttl <= 0 时用 DefaultDedupTTL。
func NewDeduper(ttl time.Duration) *Deduper {
	if ttl <= 0 {
		ttl = DefaultDedupTTL
	}
	return &Deduper{
		seen: make(map[string]time.Time),
		ttl:  ttl,
		now:  time.Now,
	}
}

// Seen 判断该消息是否已处理过，并记录首次出现。
//
// 返回 true 表示**重复**（调用方应丢弃）；false 表示首次（应处理）。
//
// 空 ID 恒返回 false（不拦）——空 ID 无法作为去重键，把它当作「已见」
// 会让所有缺 ID 的消息被静默丢弃，那是更糟的失效方向。飞书消息事件
// 必定带 message_id（parse.go 从元数据取），故实际不会走到这里。
func (d *Deduper) Seen(messageID string) bool {
	if messageID == "" {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	if ts, ok := d.seen[messageID]; ok {
		if now.Sub(ts) < d.ttl {
			return true // 仍在记忆窗口内 → 重复
		}
		// 过期：视作首次，刷新时间戳。
	}

	d.seen[messageID] = now
	d.evictLocked(now)
	return false
}

// Len 返回当前记忆的消息数（测试与观测用）。
func (d *Deduper) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// evictLocked 清理过期条目。
//
// 调用方必须持锁。全量扫描而非惰性清理：条目数受 TTL 与消息速率约束
// （10 分钟窗口），规模可控；而惰性清理需要额外的堆结构，复杂度不划算。
func (d *Deduper) evictLocked(now time.Time) {
	for id, ts := range d.seen {
		if now.Sub(ts) >= d.ttl {
			delete(d.seen, id)
		}
	}
}
