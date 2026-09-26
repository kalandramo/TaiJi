package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/bootstrap"
)

// 工具调用轮次上限的端到端验证（permission_plugin.go:115 的缺口修复）。
//
// 背景：确定性拒绝（权限拒绝）的「不要重试」只是对模型的行为指令，
// 无法在协议层强制。本上限是硬保障——模型反复重试时，轮次达上限即终止。
//
// 关键前提（已实测）：被权限**拒绝**的工具调用也计入迭代——框架在
// 权限检查之前计数（functioncall.go:320 的 IncToolIteration 先于 :342 的
// toolExecutionDecision）。故上限能挡住「重试被拒工具」的死循环。

// loopingSSE 起一个**反复请求工具**的 mock。
//
// finalizeRespected=true 时，检测到 finalization 指令（含 "call limit"）
// 就回文本——模拟「模型配合」；false 时永远请求工具——模拟病态。
func loopingSSE(t *testing.T, toolName string, finalizeRespected bool) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		finalizing := false
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "call limit") {
				finalizing = true
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		if finalizing && finalizeRespected {
			payload := map[string]any{
				"id": "f", "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": req.Model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]string{"content": "已尽力回答。"},
				}},
			}
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		payload := map[string]any{
			"id": "c", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": req.Model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"index": 0, "id": fmt.Sprintf("call_%d", n), "type": "function",
						"function": map[string]string{
							"name":      toolName,
							"arguments": `{"note":"x"}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// loopExecutor 装配一个权限拒绝一切、带上限的执行器。
func loopExecutor(t *testing.T, srvURL string, maxIter int) *Executor {
	t.Helper()
	toolSets, err := bootstrap.NewMCPSets([]bootstrap.MCPServerConfig{{
		Name:      "mockmcp",
		Transport: "stdio",
		Command:   "go",
		Args:      []string{"run", "../../testdata/mockmcp"},
		Timeout:   30 * time.Second,
	}})
	if err != nil {
		t.Fatalf("NewMCPSets: %v", err)
	}
	t.Cleanup(func() { bootstrap.CloseMCPSets(toolSets) })

	opts := testOptions(srvURL, &sseRecorder{})
	opts.ToolSets = toolSets
	opts.AllowTools = []string{"mockmcp_write_note"}
	opts.Permissions = authz.NewStaticPermissions(nil) // 空表 = 拒绝一切
	opts.MaxToolIterations = maxIter

	ex, err := NewExecutor(opts)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	t.Cleanup(ex.Close)
	return ex
}

func loopCtx() context.Context {
	ctx := authz.WithContextKind(context.Background(), authz.KindInteractive)
	return authz.WithPrincipal(ctx, authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_probe"})
}

// 模型病态重试 → 轮次达上限即终止（不死循环）。
func TestMaxToolIterations_StopsPathologicalRetry(t *testing.T) {
	srv, calls := loopingSSE(t, "mockmcp_write_note", false)
	ex := loopExecutor(t, srv.URL, 3)

	_, err := ex.Execute(loopCtx(), "loop-a", "反复试")
	got := atomic.LoadInt32(calls)

	// 上限=3 → 最多 4 次模型调用（3 轮工具 + 1 次 finalization）。
	// 允许一点余量，但必须**远小于**死循环。
	if got > 6 {
		t.Errorf("模型被调用 %d 次 —— 上限未生效（应 ≈ MaxToolIterations+1）", got)
	}
	t.Logf("✓ 病态重试被终止：modelCalls=%d err=%v", got, err)
}

// 模型配合 finalization → 优雅收尾（得到回答，非报错）。
func TestMaxToolIterations_GracefulFinalization(t *testing.T) {
	srv, calls := loopingSSE(t, "mockmcp_write_note", true)
	ex := loopExecutor(t, srv.URL, 3)

	answer, err := ex.Execute(loopCtx(), "loop-b", "反复试")

	if err != nil {
		t.Errorf("配合的模型应优雅收尾（err=nil），实际 err=%v", err)
	}
	if answer == "" {
		t.Error("优雅收尾应给出回答，实际为空")
	}
	t.Logf("✓ 优雅收尾：modelCalls=%d answer=%q", atomic.LoadInt32(calls), answer)
}

// 未设上限 → 不干预（向后兼容）。用一个「先工具、后文本」的模型验证正常任务不受影响。
func TestMaxToolIterations_UnsetPreservesBehavior(t *testing.T) {
	srv := namedToolCallSSE(t, "mockmcp_write_note", `{"note":"hi"}`)

	toolSets, err := bootstrap.NewMCPSets([]bootstrap.MCPServerConfig{{
		Name:      "mockmcp",
		Transport: "stdio",
		Command:   "go",
		Args:      []string{"run", "../../testdata/mockmcp"},
		Timeout:   30 * time.Second,
	}})
	if err != nil {
		t.Fatalf("NewMCPSets: %v", err)
	}
	t.Cleanup(func() { bootstrap.CloseMCPSets(toolSets) })

	opts := testOptions(srv.URL, &sseRecorder{})
	opts.ToolSets = toolSets
	opts.AllowTools = []string{"mockmcp_write_note"}
	// MaxToolIterations 刻意不设（默认 0 = 不限制）

	ex, err := NewExecutor(opts)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	t.Cleanup(ex.Close)

	answer, err := ex.Execute(loopCtx(), "loop-c", "写个笔记")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(answer, "Wrote:") {
		t.Errorf("未设上限时正常任务应照常执行，回答 = %q", answer)
	}
	t.Logf("✓ 未设上限不干预，回答 = %q", answer)
}
