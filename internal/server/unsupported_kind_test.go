package server_test

import (
	"context"
	"testing"
)

// 非文本消息的处理（实测缺陷修复）。
//
// 用户实测（2026-09-24 23:01）：飞书发图片 → 回复「chat: input is blank」。
// 根因：图片消息 content 里没有 text 字段 → Content 为空串 →
// 下游 ErrBlankInput → 内部错误串泄漏到用户界面。
//
// 修复后：识别出「类型不支持」，给出针对性提示，**不调用模型**。

func TestUnsupportedKind_RepliesWithoutCallingModel(t *testing.T) {
	sender := &recordingSender{}
	exec := &echoExecutor{}
	p := newCardHarness(t, sender, exec)

	msg := directMsg("om_img_1", "")
	msg.UnsupportedKind = "image"

	if err := p.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 关键断言 1：没有调用模型（省一次无意义的 API 调用 + 无 ErrBlankInput）
	if n := len(exec.inputs()); n != 0 {
		t.Errorf("非文本消息不应调用模型，实际调用了 %d 次", n)
	}

	// 关键断言 2：用户收到**针对性**提示，不是内部错误串
	calls := sender.snapshot()
	if len(calls) != 1 {
		t.Fatalf("应发 1 条提示消息，实际 %d 条", len(calls))
	}
	text := calls[0].Text
	if !contains(text, "图片") {
		t.Errorf("提示应说明是图片消息，实际: %q", text)
	}
	if contains(text, "input is blank") {
		t.Errorf("不应泄漏内部错误串，实际: %q", text)
	}
	t.Logf("✓ 图片消息回复: %q（未调用模型）", text)
}

// 各类非文本消息都有对应文案。
func TestUnsupportedKind_MessagePerType(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{"image", "图片"},
		{"file", "文件"},
		{"audio", "语音"},
		{"media", "视频"},
		{"sticker", "表情包"},
		{"weird_type", "weird_type"}, // 未知类型兜底带原名
	}
	for _, c := range cases {
		sender := &recordingSender{}
		p := newCardHarness(t, sender, &echoExecutor{})
		msg := directMsg("om_"+c.kind, "")
		msg.UnsupportedKind = c.kind
		if err := p.Handle(context.Background(), msg); err != nil {
			t.Fatalf("%s: Handle: %v", c.kind, err)
		}
		calls := sender.snapshot()
		if len(calls) != 1 || !contains(calls[0].Text, c.want) {
			t.Errorf("%s: 提示应含 %q，实际 %v", c.kind, c.want, calls)
		}
	}
	t.Log("✓ 6 种类型文案正确（含未知类型兜底）")
}

// 回归防护：正常文本消息**不受影响**（仍走模型）。
func TestUnsupportedKind_TextStillGoesToModel(t *testing.T) {
	sender := &recordingSender{}
	exec := &echoExecutor{}
	p := newCardHarness(t, sender, exec)

	if err := p.Handle(context.Background(), directMsg("om_txt", "查询 IP")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n := len(exec.inputs()); n != 1 {
		t.Errorf("文本消息应调用模型 1 次，实际 %d 次", n)
	}
}

// ── 反向验证：UnsupportedKind 为空时行为不变 ──
//
// 若把「类型不支持」判定误伤到文本消息（如按 message_type 一刀切），
// 这条会失败。
func TestUnsupportedKind_EmptyKindIsNormalPath(t *testing.T) {
	sender := &recordingSender{}
	exec := &echoExecutor{}
	p := newCardHarness(t, sender, exec)

	msg := directMsg("om_normal", "正常问题")
	// UnsupportedKind 留空 = 文本路径
	if err := p.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if n := len(exec.inputs()); n != 1 {
		t.Errorf("UnsupportedKind 为空时应走正常路径，实际模型调用 %d 次", n)
	}
	if calls := sender.snapshot(); len(calls) != 2 {
		t.Errorf("文本路径应 2 次出站（占位+更新），实际 %d", len(calls))
	}
}
