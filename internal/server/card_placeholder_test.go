package server_test

import (
	"context"
	"testing"
)

// 卡片占位文本与降级边界（实测缺陷修复）。
//
// 用户实测（2026-09-24 23:01 截图）：
//   1. 卡片消息发出后有一段**空白框**（创建时内容为空，首个 chunk 未到）
//   2. 「抱歉，处理时出错：chat: input is blank」出现了**两次**——
//      一次在卡片里（已编辑），一次是独立的文本消息

// 修复 1：占位文本在创建卡片时写入（不是发消息后补）。
func TestCardStream_PlaceholderPassedAtCreation(t *testing.T) {
	sender := &fakeStreamingSender{}
	exec := &chunkingExecutor{chunks: []string{"回答内容足够长以触发阈值推送"}}
	p := newCardHarness(t, sender, exec)

	if err := p.Handle(context.Background(), directMsg("om_ph_1", "问题")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	sender.mu.Lock()
	ph := sender.placeholder
	sender.mu.Unlock()

	if ph == "" {
		t.Error("创建卡片时必须传占位文本——否则用户看到空白框")
	}
	if ph != "思考中…" {
		t.Errorf("占位文本应为默认值，实际 %q", ph)
	}
	t.Logf("✓ 卡片创建时占位文本 = %q（无空白框）", ph)
}

// 修复 2：卡片已发出后执行失败 → **不**降级（避免重复消息）。
func TestCardStream_NoFallbackAfterCardSent(t *testing.T) {
	sender := &fakeStreamingSender{}
	exec := &failingExecutor{err: errBlankLike}
	p := newCardHarness(t, sender, exec)

	if err := p.Handle(context.Background(), directMsg("om_nf_1", "问题")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	_, closed, textCalls := sender.snapshot()
	if !closed {
		t.Error("卡片应被 Close（即使执行失败）")
	}
	// 关键断言：**没有**额外的文本消息
	if len(textCalls) != 0 {
		t.Errorf("卡片已发出后不应再发文本消息（会产生两条），实际 %d 条: %v",
			len(textCalls), textCalls)
	}
	t.Log("✓ 卡片已发出后执行失败：只更新卡片，未重复发文本")
}

// 反向验证：卡片**未**发出时失败 → 仍应降级（用户不能什么都收不到）。
func TestCardStream_FallbackStillWorksBeforeCardSent(t *testing.T) {
	sender := &fakeStreamingSender{startErr: errStartFail}
	exec := &echoExecutor{}
	p := newCardHarness(t, sender, exec)

	if err := p.Handle(context.Background(), directMsg("om_bf_1", "问题")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	_, _, textCalls := sender.snapshot()
	if len(textCalls) != 2 {
		t.Fatalf("卡片未发出时失败应降级到文本（2 次调用），实际 %d: %v",
			len(textCalls), textCalls)
	}
	t.Logf("✓ 卡片未发出时降级正常: %d 次文本调用", len(textCalls))
}

// ── 测试辅助 ──

var errBlankLike = errTest("execute: chat: input is blank")
var errStartFail = errTest("cardkit unavailable")

type errTest string

func (e errTest) Error() string { return string(e) }

// failingExecutor 总是返回错误（模拟 ErrBlankInput 等执行失败）。
type failingExecutor struct{ err error }

func (f *failingExecutor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	return "", f.err
}

func (f *failingExecutor) ExecuteStream(ctx context.Context, sessionID, input string, onChunk func(string)) (string, error) {
	return "", f.err
}
