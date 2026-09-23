package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/bootstrap"
)

// 本文件闭合 issue #2 的 AC-1（流式逐块）与 AC-3（多轮历史）——
// 这两条此前只靠人工手动运行验证，仓库内零自动化覆盖（L2 审查发现）。

// sseRecorder 是一个记录型 io.Writer：它把每次 Write 单独记下来，
// 使「逐块输出」成为可断言的事实（一次性整段 = 1 次 Write）。
type sseRecorder struct {
	mu     sync.Mutex
	writes []string
}

func (r *sseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, string(p))
	return len(p), nil
}

func (r *sseRecorder) chunkCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.writes)
}

func (r *sseRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.writes, "")
}

// mockSSE 返回一个 OpenAI 兼容的流式端点，并把每次请求的 message 条数
// 记到 msgCounts（AC-3 的观察点）。
func mockSSE(t *testing.T, chunks []string) (*httptest.Server, *[]int) {
	t.Helper()
	var mu sync.Mutex
	msgCounts := &[]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		*msgCounts = append(*msgCounts, len(req.Messages))
		mu.Unlock()

		if !req.Stream {
			// 非流式：整段返回（用于对照）
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "c1", "object": "chat.completion", "model": req.Model,
				"choices": []map[string]any{{
					"index":         0,
					"message":       map[string]string{"role": "assistant", "content": strings.Join(chunks, "")},
					"finish_reason": "stop",
				}},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			payload := map[string]any{
				"id": "c1", "object": "chat.completion.chunk", "model": req.Model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": c}}},
			}
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(10 * time.Millisecond) // 让分块在时间上可区分
		}
		final := map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}},
		}
		b, _ := json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, msgCounts
}

func testOptions(srvURL string, out *sseRecorder) Options {
	return Options{
		Config:    modelConfigFor(srvURL),
		AppName:   "test",
		UserID:    "u1",
		SessionID: fmt.Sprintf("s-%d", time.Now().UnixNano()),
		Out:       out,
		Echo:      &bytes.Buffer{},
	}
}

// modelConfigFor 构造指向 httptest 端点的模型配置。
func modelConfigFor(srvURL string) bootstrap.ModelConfig {
	return bootstrap.ModelConfig{
		Name:    "test-model",
		APIKey:  "test-key",
		BaseURL: srvURL,
	}
}

// --- AC-1: 流式逐块输出 ---

func TestRun_StreamsInMultipleChunks(t *testing.T) {
	srv, _ := mockSSE(t, []string{"你好", "，", "世界"})

	out := &sseRecorder{}
	opts := testOptions(srv.URL, out)

	if err := Run(context.Background(), strings.NewReader("hi\nexit\n"), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 内容完整（Run 在每轮后补一个换行，故用 TrimRight 比较正文）
	if got := strings.TrimRight(out.String(), "\n"); got != "你好，世界" {
		t.Errorf("output = %q, want %q", got, "你好，世界")
	}
	// 分块：3 个内容块应产生 >=3 次 Write（一次性整段只会是 1 次）
	if n := out.chunkCount(); n < 3 {
		t.Errorf("chunk count = %d, want >=3 (streaming must not emit as one block)", n)
	}
}

// --- AC-3: 多轮历史 ---

func TestRun_MultiTurnCarriesHistory(t *testing.T) {
	srv, msgCounts := mockSSE(t, []string{"ok"})

	out := &sseRecorder{}
	opts := testOptions(srv.URL, out)

	// 同一 session 内连问两句
	if err := Run(context.Background(), strings.NewReader("第一句\n第二句\nexit\n"), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := *msgCounts
	if len(got) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(got))
	}
	if got[0] != 1 {
		t.Errorf("first request had %d messages, want 1 (only the new user msg)", got[0])
	}
	// 第二轮应含：user1 + assistant1 + user2 = 3 条
	if got[1] <= got[0] {
		t.Errorf("second request had %d messages, want > %d (history must accumulate)",
			got[1], got[0])
	}
}

// --- AC-2: 空模型名（chat 层）---

func TestRun_EmptyModelNameFails(t *testing.T) {
	out := &sseRecorder{}
	opts := Options{
		Config: modelConfigFor("http://127.0.0.1:1/v1"),
		Out:    out,
		Echo:   &bytes.Buffer{},
	}
	opts.Config.Name = "" // 显式清空

	err := Run(context.Background(), strings.NewReader("hi\n"), opts)
	if err == nil {
		t.Fatal("Run with empty model name returned nil error")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error %q should mention the model name", err.Error())
	}
}

// --- 边界：exit 立即退出，不发请求 ---

func TestRun_ExitDoesNotCallProvider(t *testing.T) {
	srv, msgCounts := mockSSE(t, []string{"x"})

	out := &sseRecorder{}
	opts := testOptions(srv.URL, out)

	if err := Run(context.Background(), strings.NewReader("exit\n"), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*msgCounts) != 0 {
		t.Errorf("provider was called %d times after immediate exit, want 0", len(*msgCounts))
	}
}

// --- 边界：空行被跳过 ---

func TestRun_BlankLinesSkipped(t *testing.T) {
	srv, msgCounts := mockSSE(t, []string{"ok"})

	out := &sseRecorder{}
	opts := testOptions(srv.URL, out)

	if err := Run(context.Background(), strings.NewReader("\n\n  \n你好\nexit\n"), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*msgCounts) != 1 {
		t.Errorf("provider called %d times, want 1 (blank lines must be skipped)", len(*msgCounts))
	}
}
