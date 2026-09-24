package chat

import (
	"context"
	"strings"
	"testing"
)

// 流式 chunk 外传（issue #10 Wave 2）。
//
// 设计取舍：加 ExecuteStream 方法而非改 Execute 签名。
//   - 既有 Execute 调用点（8+ 处）零改动
//   - 5 个测试 fake 只需多实现一个方法
//   - Execute 内部委托 ExecuteStream(nil)，语义完全不变

// 流式回调应逐块收到内容，且与完整回答一致。
func TestExecuteStream_ChunksMatchFullAnswer(t *testing.T) {
	srv, _ := mockSSE(t, []string{"你好", "，", "世界"})
	ex := newTestExecutor(t, testOptions(srv.URL, &sseRecorder{}))

	var chunks []string
	answer, err := ex.ExecuteStream(context.Background(), "stream-1", "hi", func(c string) {
		chunks = append(chunks, c)
	})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	if answer != "你好，世界" {
		t.Errorf("完整回答 = %q, want %q", answer, "你好，世界")
	}
	if len(chunks) == 0 {
		t.Fatal("未收到任何 chunk —— 流式未外传")
	}
	// chunk 拼接必须等于完整回答（否则调用方展示的内容会与最终态不一致）
	if got := strings.Join(chunks, ""); got != answer {
		t.Errorf("chunk 拼接 = %q, want %q（必须与完整回答一致）", got, answer)
	}
	t.Logf("✓ 收到 %d 个 chunk，拼接 = %q", len(chunks), strings.Join(chunks, ""))
}

// onChunk 为 nil 时行为与 Execute 完全一致（向后兼容）。
func TestExecuteStream_NilCallbackBehavesLikeExecute(t *testing.T) {
	srv, _ := mockSSE(t, []string{"abc"})
	ex := newTestExecutor(t, testOptions(srv.URL, &sseRecorder{}))

	got, err := ex.ExecuteStream(context.Background(), "stream-2", "hi", nil)
	if err != nil {
		t.Fatalf("ExecuteStream(nil): %v", err)
	}
	if got != "abc" {
		t.Errorf("回答 = %q, want abc", got)
	}
}

// Execute 委托 ExecuteStream —— 两者结果必须一致。
func TestExecute_DelegatesToStream(t *testing.T) {
	srv, _ := mockSSE(t, []string{"委托", "测试"})
	ex := newTestExecutor(t, testOptions(srv.URL, &sseRecorder{}))

	got, err := ex.Execute(context.Background(), "deleg-1", "hi")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "委托测试" {
		t.Errorf("Execute 结果 = %q（应与 ExecuteStream 一致）", got)
	}
}

// 参数校验在两条路径上一致（空 sessionID / 空输入）。
func TestExecuteStream_ValidatesParams(t *testing.T) {
	srv, _ := mockSSE(t, []string{"x"})
	ex := newTestExecutor(t, testOptions(srv.URL, &sseRecorder{}))

	if _, err := ex.ExecuteStream(context.Background(), "", "hi", nil); err == nil {
		t.Error("空 sessionID 应报错（与 Execute 一致）")
	}
	if _, err := ex.ExecuteStream(context.Background(), "s", "  ", nil); err == nil {
		t.Error("空输入应报错（与 Execute 一致）")
	}
}
