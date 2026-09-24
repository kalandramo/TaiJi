package server

import (
	"strings"
	"sync"
	"time"
)

// 流式更新的节流（issue #10）。
//
// 问题：模型可能逐字符产出 chunk，每个都调飞书 CardKit 会产生大量请求
// （限流风险 + 网络开销）。实测 mock 端点下 3 个 chunk 对应 3 次调用，
// 真实模型的长回答可能产生数百个。
//
// 策略：双阈值聚合——时间与长度任一满足即触发刷新。
//
//	最小间隔 200ms   —— 飞书卡片更新有频率限制；200ms 肉眼已是流畅打字
//	最小增量 16 字符 —— 避免短间隔内发微小更新
//	强制刷新         —— 流结束时（Flush）保证最终内容完整
//
// 为什么放管道层而非渠道层：节流是**编排策略**（何时发），不是渠道能力
// （怎么发）。换渠道时节流参数可能需要调整，而渠道实现不该关心这个。

const (
	// streamMinInterval 是两次卡片更新之间的最小间隔。
	streamMinInterval = 200 * time.Millisecond
	// streamMinDelta 是触发更新所需的最小新增字符数。
	streamMinDelta = 16
)

// chunkThrottle 聚合流式 chunk，按阈值决定何时真正推送。
//
// 非并发安全：它被管道在单次消息处理中顺序使用（串行化器保证同会话
// 不并发）。若将来并发使用，需加锁——但不预先加锁，避免掩盖误用。
type chunkThrottle struct {
	mu       sync.Mutex
	buf      strings.Builder // 累积的完整文本
	lastSent string        // 上次推送的文本（用于算增量）
	lastAt   time.Time     // 上次推送时间
	force    bool          // 下次 ShouldFlush 必返回 true
}

// newChunkThrottle 构造节流器。now 可注入（测试用）。
func newChunkThrottle() *chunkThrottle {
	return &chunkThrottle{}
}

// Add 追加一个 chunk，返回「当前累积的完整文本」。
//
// 调用方拿到完整文本后，用 ShouldFlush 判断是否该推送给平台。
// 返回完整文本而非增量：CardKit 的 Content 接口语义是"更新后的文本内容"
// （幂等），传完整文本使重试安全。
func (t *chunkThrottle) Add(chunk string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.WriteString(chunk)
	return t.buf.String()
}

// ShouldFlush 判断当前是否应推送。
//
// 条件（任一满足）：
//   - force（流结束或显式请求）
//   - 距上次推送 >= streamMinInterval
//   - 自上次推送的新增字符数 >= streamMinDelta
//
// 首次调用（lastAt 为零值）总是返回 true——首块应立即显示，
// 否则用户会看到空卡片停留一个节流周期。
func (t *chunkThrottle) ShouldFlush(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.force {
		return true
	}
	current := t.buf.String()
	if current == t.lastSent {
		return false // 无新增，不推
	}
	if t.lastAt.IsZero() {
		return true // 首块立即显示
	}
	if now.Sub(t.lastAt) >= streamMinInterval {
		return true
	}
	if len(current)-len(t.lastSent) >= streamMinDelta {
		return true
	}
	return false
}

// MarkSent 记录一次成功推送。
func (t *chunkThrottle) MarkSent(text string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastSent = text
	t.lastAt = now
}

// Force 让下次 ShouldFlush 必返回 true（流结束时用）。
func (t *chunkThrottle) Force() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.force = true
}

// Text 返回当前累积的完整文本。
func (t *chunkThrottle) Text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
