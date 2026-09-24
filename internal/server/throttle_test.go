package server

import (
	"strings"
	"testing"
	"time"
)

// 流式节流（issue #10）。
//
// 为什么需要：模型逐字符产出 chunk，每个都调飞书 CardKit 会产生大量
// 请求（限流风险 + 开销）。双阈值聚合：时间 200ms 或增量 16 字符。

func TestThrottle_FirstChunkAlwaysFlushes(t *testing.T) {
	// 首块必须立即显示——否则用户看到空卡片停留一个节流周期。
	th := newChunkThrottle()
	th.Add("首块")
	if !th.ShouldFlush(time.Now()) {
		t.Error("首块应立即可推送（lastAt 为零值）")
	}
}

func TestThrottle_NoNewContentDoesNotFlush(t *testing.T) {
	th := newChunkThrottle()
	th.Add("abc")
	now := time.Now()
	th.MarkSent("abc", now)

	// 无新增 → 不推（即使时间到了）
	if th.ShouldFlush(now.Add(10 * time.Second)) {
		t.Error("无新增内容时不应推送")
	}
}

func TestThrottle_IntervalTriggersFlush(t *testing.T) {
	th := newChunkThrottle()
	now := time.Now()
	th.Add("a")
	th.MarkSent("a", now)

	th.Add("b") // 增量 1 字符，不足 16

	// 未到间隔 → 不推
	if th.ShouldFlush(now.Add(50 * time.Millisecond)) {
		t.Error("未到最小间隔且增量不足时不应推送")
	}
	// 超过间隔 → 推
	if !th.ShouldFlush(now.Add(streamMinInterval + time.Millisecond)) {
		t.Error("超过最小间隔应推送")
	}
}

func TestThrottle_DeltaTriggersFlush(t *testing.T) {
	th := newChunkThrottle()
	now := time.Now()
	th.Add("start")
	th.MarkSent("start", now)

	// 增量 >= 16 字符 → 立即推（不等间隔）
	th.Add(strings.Repeat("x", streamMinDelta))
	if !th.ShouldFlush(now.Add(time.Millisecond)) {
		t.Error("增量达到阈值应推送（不等间隔）")
	}
}

func TestThrottle_ForceAlwaysFlushes(t *testing.T) {
	th := newChunkThrottle()
	now := time.Now()
	th.Add("x")
	th.MarkSent("x", now)
	th.Add("y")

	th.Force()
	if !th.ShouldFlush(now.Add(time.Millisecond)) {
		t.Error("Force 后应推送（流结束时保证最终内容完整）")
	}
}

func TestThrottle_AddAccumulates(t *testing.T) {
	th := newChunkThrottle()
	th.Add("你好")
	th.Add("，")
	got := th.Add("世界")
	if got != "你好，世界" {
		t.Errorf("累积文本 = %q, want 你好，世界", got)
	}
	if th.Text() != "你好，世界" {
		t.Errorf("Text() = %q", th.Text())
	}
}
