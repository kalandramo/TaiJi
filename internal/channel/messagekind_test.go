package channel

import "testing"

// 出站消息形态（issue #10：卡片流式渲染）。
//
// 设计取舍：在 SendOptions 上加 Kind 字段，而非新增 SendCard 方法或
// 把 text string 换成 Message 结构体。理由（详见设计文档 §3.3）：
//   - 加字段：生产调用点仅 1 处，8 个测试夹具只改 fake 内部
//   - 新方法：接口膨胀，调用方需分支
//   - 换结构体：破坏面最大
//
// 向后兼容：Kind 空值 = KindText（既有行为）。

func TestMessageKind_ZeroValueIsText(t *testing.T) {
	// 零值必须是文本——否则既有调用（未设 Kind）会意外走卡片路径。
	var opts SendOptions
	if got := opts.EffectiveKind(); got != KindText {
		t.Errorf("零值的 EffectiveKind() = %q, want %q（既有行为不可变）", got, KindText)
	}
}

func TestMessageKind_ExplicitCard(t *testing.T) {
	opts := SendOptions{Kind: KindCard}
	if got := opts.EffectiveKind(); got != KindCard {
		t.Errorf("EffectiveKind() = %q, want %q", got, KindCard)
	}
}

func TestMessageKind_ExplicitText(t *testing.T) {
	opts := SendOptions{Kind: KindText}
	if got := opts.EffectiveKind(); got != KindText {
		t.Errorf("EffectiveKind() = %q, want %q", got, KindText)
	}
}

// 未知值 → 回退文本（fail-safe：宁可不渲染卡片，也不发一条平台不认的消息）。
func TestMessageKind_UnknownFallsBackToText(t *testing.T) {
	opts := SendOptions{Kind: MessageKind("bogus")}
	if got := opts.EffectiveKind(); got != KindText {
		t.Errorf("未知 Kind 的 EffectiveKind() = %q, want %q（fail-safe 回退）", got, KindText)
	}
}
