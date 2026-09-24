package server_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 卡片流式的管道编排（issue #10 Wave 2）。
//
// 验证两条路径 + 降级：
//   A. 渠道支持流式 → 走卡片，逐块更新（AC-2）
//   B. 渠道不支持 → 走文本（既有行为）
//   C. 卡片失败 → 降级文本（AC-5）

// fakeStreamingSender 记录卡片流式调用。
type fakeStreamingSender struct {
	mu          sync.Mutex
	updates     []string
	closed      bool
	closeText   string
	startErr    error
	updateErr   error
	placeholder string // 创建卡片时传入的占位文本

	// 文本路径的记录（降级时用）
	textCalls []string
}

func (f *fakeStreamingSender) SendMessage(ctx context.Context, to, text string, opts channel.SendOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.textCalls = append(f.textCalls, text)
	if opts.MessageID != "" {
		return opts.MessageID, nil
	}
	return "om_placeholder", nil
}

func (f *fakeStreamingSender) StartCardStream(ctx context.Context, to, placeholder string, opts channel.SendOptions) (channel.CardStream, error) {
	f.mu.Lock()
	f.placeholder = placeholder
	f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &fakeStream{f: f}, nil
}

func (f *fakeStreamingSender) snapshot() (updates []string, closed bool, textCalls []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updates...), f.closed, append([]string(nil), f.textCalls...)
}

type fakeStream struct{ f *fakeStreamingSender }

func (s *fakeStream) Update(ctx context.Context, text string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.updateErr != nil {
		return s.f.updateErr
	}
	s.f.updates = append(s.f.updates, text)
	return nil
}

func (s *fakeStream) Close(ctx context.Context, finalText string) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.closed = true
	s.f.closeText = finalText
	s.f.updates = append(s.f.updates, finalText)
	return nil
}

var _ channel.StreamingSender = (*fakeStreamingSender)(nil)

// chunkingExecutor 产出多个 chunk（模拟流式生成）。
type chunkingExecutor struct {
	chunks []string
}

func (e *chunkingExecutor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	return e.ExecuteStream(ctx, sessionID, input, nil)
}

func (e *chunkingExecutor) ExecuteStream(ctx context.Context, sessionID, input string, onChunk func(string)) (string, error) {
	var full string
	for _, c := range e.chunks {
		full += c
		if onChunk != nil {
			onChunk(c)
		}
	}
	return full, nil
}

func newCardHarness(t *testing.T, sender channel.Sender, exec server.Executor) *server.Pipeline {
	t.Helper()
	p, err := server.New(server.Config{
		Sender:   sender,
		Executor: exec,
		Gate:     allowAllGate(),
		Route:    channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return p
}

// ===== A. 支持流式的渠道 → 走卡片 =====

func TestCardStream_UsesCardWhenSupported(t *testing.T) {
	sender := &fakeStreamingSender{}
	// 多个 chunk，触发节流后的多次更新
	exec := &chunkingExecutor{chunks: []string{
		"第一块内容足够长以触发阈值", "第二块内容也足够长触发阈值", "第三块",
	}}
	p := newCardHarness(t, sender, exec)

	if err := p.Handle(context.Background(), directMsg("om_card_1", "问题")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	updates, closed, textCalls := sender.snapshot()
	if !closed {
		t.Error("卡片流必须被 Close（否则卡片卡在生成中）")
	}
	if len(textCalls) != 0 {
		t.Errorf("走卡片路径时不应发文本消息，实际 %d 次: %v", len(textCalls), textCalls)
	}
	if len(updates) < 2 {
		t.Errorf("应有多次卡片更新（AC-2），实际 %d 次: %v", len(updates), updates)
	}
	t.Logf("✓ 卡片更新 %d 次，最终 = %q", len(updates), updates[len(updates)-1])
}

// ===== B. 不支持的渠道 → 走文本（既有行为）=====

func TestCardStream_FallsBackToTextWhenUnsupported(t *testing.T) {
	sender := &recordingSender{} // 只实现 Sender，不实现 StreamingSender
	exec := &echoExecutor{}
	p := newCardHarness(t, sender, exec)

	if err := p.Handle(context.Background(), directMsg("om_text_1", "问题")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	calls := sender.snapshot()
	if len(calls) != 2 {
		t.Fatalf("文本路径应有 2 次调用（占位+更新），实际 %d", len(calls))
	}
	t.Logf("✓ 不支持流式时走文本路径: %d 次调用", len(calls))
}

// ===== C. 卡片启动失败 → 降级文本（AC-5）=====

func TestCardStream_FallsBackWhenStartFails(t *testing.T) {
	sender := &fakeStreamingSender{startErr: errors.New("cardkit unavailable")}
	exec := &echoExecutor{}
	p := newCardHarness(t, sender, exec)

	if err := p.Handle(context.Background(), directMsg("om_fb_1", "问题")); err != nil {
		t.Fatalf("Handle 不应返回错误（卡片失败应降级，非中断）: %v", err)
	}

	_, _, textCalls := sender.snapshot()
	if len(textCalls) != 2 {
		t.Fatalf("降级后应走文本路径（2 次调用），实际 %d: %v", len(textCalls), textCalls)
	}
	t.Logf("✓ 卡片失败降级到文本: %v", textCalls)
}

// 卡片更新失败不中断生成（流结束时仍 Close）。
func TestCardStream_UpdateFailureDoesNotAbort(t *testing.T) {
	sender := &fakeStreamingSender{updateErr: errors.New("update failed")}
	exec := &chunkingExecutor{chunks: []string{"内容足够长的第一块内容啊", "第二块"}}
	p := newCardHarness(t, sender, exec)

	if err := p.Handle(context.Background(), directMsg("om_up_1", "问题")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	_, closed, _ := sender.snapshot()
	if !closed {
		t.Error("即使更新失败，流结束也应 Close（避免卡片卡住）")
	}
}
