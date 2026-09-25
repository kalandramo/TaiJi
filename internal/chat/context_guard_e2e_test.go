package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/bootstrap"
)

// 上下文级降权的端到端验证（issue #6 缺口 1 的接线）。
//
// **不 mock 中间层**：走真实 MCP server（testdata/mockmcp）+ 真实框架链路，
// 验证「来源上下文 → 工具是否执行」的完整路径。
//
// 与单元测试（context_guard_test.go）的分工：单测证明判定逻辑正确，
// e2e 证明判定**真的被框架调用**（插件注册、ctx 透传、metadata 传递）。
// 前者绿不代表后者绿——这正是本文件存在的理由。

// namedToolCallSSE 起一个会**指定工具名**触发 tool_call 的端点。
//
// 与 toolCallSSE 的差异：工具名由参数给定，不依赖 tools 列表顺序
// （列表顺序不稳定，用它会让测试 flaky）。
func namedToolCallSSE(t *testing.T, toolName, argsJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		hasToolResult := false
		toolContent := ""
		for _, m := range req.Messages {
			if m.Role == "tool" {
				hasToolResult = true
				toolContent = m.Content
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		if !hasToolResult {
			payload := map[string]any{
				"id": "c1", "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": req.Model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"index": 0, "id": "call_1", "type": "function",
							"function": map[string]string{
								"name":      toolName,
								"arguments": argsJSON,
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
			return
		}

		reply := "完成"
		if toolContent != "" {
			reply = "工具返回：" + toolContent
		}
		payload := map[string]any{
			"id": "c2", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": req.Model,
			"choices": []map[string]any{{
				"index": 0, "delta": map[string]string{"content": reply},
			}},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		final := map[string]any{
			"id": "c2", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": req.Model,
			"choices": []map[string]any{{
				"index": 0, "delta": map[string]string{}, "finish_reason": "stop",
			}},
		}
		b2, _ := json.Marshal(final)
		fmt.Fprintf(w, "data: %s\n\n", b2)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ctxGuardExecutor 装配一个带 mockmcp 的 Executor（echo 只读 + write_note 写）。
func ctxGuardExecutor(t *testing.T, srvURL string) *Executor {
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
	opts.AllowTools = []string{"mockmcp_echo", "mockmcp_write_note"}

	ex, err := NewExecutor(opts)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	t.Cleanup(ex.Close)
	return ex
}

// 渠道上下文 + 写工具 → 工具不执行（回答不含 Wrote:）。
func TestContextGuardE2E_ChannelWriteBlocked(t *testing.T) {
	srv := namedToolCallSSE(t, "mockmcp_write_note", `{"note":"hi"}`)
	ex := ctxGuardExecutor(t, srv.URL)

	ctx := authz.WithContextKind(context.Background(), authz.KindChannel)

	answer, err := ex.Execute(ctx, "cg-write", "写个笔记")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if contains(answer, "Wrote:") {
		t.Errorf("渠道上下文的写操作不应执行（回答含工具返回值），回答 = %q", answer)
	}
	t.Logf("✓ 渠道写操作被拦，回答 = %q", answer)
}

// 渠道上下文 + 只读工具 → 工具照常执行（不误伤）。
func TestContextGuardE2E_ChannelReadOnlyAllowed(t *testing.T) {
	srv := namedToolCallSSE(t, "mockmcp_echo", `{"message":"你好"}`)
	ex := ctxGuardExecutor(t, srv.URL)

	ctx := authz.WithContextKind(context.Background(), authz.KindChannel)

	answer, err := ex.Execute(ctx, "cg-read", "用 echo 说你好")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(answer, "Echo:") {
		t.Errorf("渠道上下文的只读操作应执行（回答含工具返回值），回答 = %q", answer)
	}
	t.Logf("✓ 渠道只读操作放行，回答 = %q", answer)
}

// 可写上下文 + 写工具 → 工具照常执行（不误伤）。
func TestContextGuardE2E_InteractiveWriteAllowed(t *testing.T) {
	srv := namedToolCallSSE(t, "mockmcp_write_note", `{"note":"hi"}`)
	ex := ctxGuardExecutor(t, srv.URL)

	ctx := authz.WithContextKind(context.Background(), authz.KindInteractive)

	answer, err := ex.Execute(ctx, "cg-interactive", "写个笔记")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(answer, "Wrote:") {
		t.Errorf("可写上下文的写操作应执行（回答含工具返回值），回答 = %q", answer)
	}
	t.Logf("✓ 可写上下文写操作放行，回答 = %q", answer)
}

// 缺失 ContextKind + 写工具 → fail-closed 拒绝。
func TestContextGuardE2E_MissingKindWriteBlocked(t *testing.T) {
	srv := namedToolCallSSE(t, "mockmcp_write_note", `{"note":"hi"}`)
	ex := ctxGuardExecutor(t, srv.URL)

	// 注意：不走 chat.Run（它会注入 KindInteractive），直接用 Execute。
	answer, err := ex.Execute(context.Background(), "cg-nokind", "写个笔记")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if contains(answer, "Wrote:") {
		t.Errorf("缺失 ContextKind 的写操作应 fail-closed 拒绝，回答 = %q", answer)
	}
	t.Logf("✓ 缺 kind 写操作被拦，回答 = %q", answer)
}
