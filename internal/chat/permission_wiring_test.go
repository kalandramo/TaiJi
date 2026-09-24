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

// 用户级权限插件的接线验证（方案 A：静态配置）。
//
// 要证明：chat.Options.Permissions 被真正挂到 runner 上，判定按
// ctx 里的 Principal 生效。
//
// 为什么必须端到端：装配点（newRunner）与消费点（框架调用 beforeTool）
// 之间隔着 SDK。单元测试证明不了"插件真的被注册了"。

// recordingPermissionSource 记录被查询的 (主体, 工具)，并返回预设结论。
type recordingPermissionSource struct {
	queries []string
	allow   bool
}

func (s *recordingPermissionSource) Allowed(_ context.Context, p authz.Principal, toolName string) (bool, error) {
	s.queries = append(s.queries, p.ID+"|"+toolName)
	return s.allow, nil
}

// toolCallSSE 起一个会触发 tool_call 的端点（复用 mockmcp 的 echo）。
//
// 行为：请求带 tools 且尚无 tool 结果 → 回 tool_call；
// 否则回普通文本（结束循环）。
func toolCallSSE(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
			Tools  []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
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
		var toolContent string
		for _, m := range req.Messages {
			if m.Role == "tool" {
				hasToolResult = true
			}
		}
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolContent = m.Content
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		if len(req.Tools) > 0 && !hasToolResult {
			name := req.Tools[0].Function.Name
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
								"name":      name,
								"arguments": `{"message":"你好"}`,
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

		// 已有工具结果 → 把它并入最终回答（对齐 testdata/mockopenai 的行为）。
		// 这样「工具是否真的执行」可从最终回答直接观测。
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

// permTestExecutor 装配一个带 mockmcp 的 Executor，注入指定权限源。
func permTestExecutor(t *testing.T, srvURL string, src authz.PermissionSource) *Executor {
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
	opts.AllowTools = []string{"mockmcp_echo"}
	opts.Permissions = src

	ex, err := NewExecutor(opts)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	t.Cleanup(ex.Close)
	return ex
}

// 授权用户 → 工具真的执行（回答含 mockmcp 的真实返回值）。
func TestPermissionWiring_AllowedUserCanUseTool(t *testing.T) {
	srv := toolCallSSE(t)
	src := &recordingPermissionSource{allow: true}
	ex := permTestExecutor(t, srv.URL, src)

	ctx := authz.WithPrincipal(context.Background(),
		authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_alice"})

	answer, err := ex.Execute(ctx, "perm-allow", "用 echo 说你好")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(src.queries) == 0 {
		t.Fatal("权限源未被查询 —— 插件没有挂到 runner 上")
	}
	if !contains(answer, "Echo:") {
		t.Errorf("授权用户应能执行工具（回答应含工具返回值），回答 = %q", answer)
	}
	t.Logf("✓ 授权用户执行成功，查询记录 = %v", src.queries)
}

// 未授权用户 → 工具不执行（回答不含工具返回值）。
func TestPermissionWiring_DeniedUserBlocked(t *testing.T) {
	srv := toolCallSSE(t)
	src := &recordingPermissionSource{allow: false}
	ex := permTestExecutor(t, srv.URL, src)

	ctx := authz.WithPrincipal(context.Background(),
		authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_bob"})

	answer, err := ex.Execute(ctx, "perm-deny", "用 echo 说你好")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if contains(answer, "Echo:") {
		t.Errorf("未授权用户却执行了工具（回答含工具返回值），回答 = %q", answer)
	}
	if len(src.queries) == 0 {
		t.Error("权限源应被查询（拒绝也是查询结果）")
	}
	t.Logf("✓ 未授权被拦，回答 = %q", answer)
}

// 查询到的身份必须是 ctx 里注入的那个（防"查询了但用的是错误身份"）。
func TestPermissionWiring_QueriesCorrectPrincipal(t *testing.T) {
	srv := toolCallSSE(t)
	src := &recordingPermissionSource{allow: true}
	ex := permTestExecutor(t, srv.URL, src)

	ctx := authz.WithPrincipal(context.Background(),
		authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_specific_user"})

	if _, err := ex.Execute(ctx, "perm-ident", "用 echo 说你好"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	want := "ws1:feishu:ou_specific_user|mockmcp_echo"
	found := false
	for _, q := range src.queries {
		if q == want {
			found = true
		}
	}
	if !found {
		t.Errorf("查询记录 = %v，应含 %q", src.queries, want)
	}
}

// 未配 Permissions → 保持既有行为（向后兼容）。
func TestPermissionWiring_NilSourceKeepsLegacyBehaviour(t *testing.T) {
	srv := toolCallSSE(t)

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
	opts.AllowTools = []string{"mockmcp_echo"}
	// 不设 Permissions

	ex, err := NewExecutor(opts)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	t.Cleanup(ex.Close)

	answer, err := ex.Execute(context.Background(), "perm-legacy", "用 echo 说你好")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !contains(answer, "Echo:") {
		t.Errorf("未配权限源时应保持既有行为（工具可执行），回答 = %q", answer)
	}
	t.Logf("✓ 向后兼容，回答 = %q", answer)
}

// contains 是包内测试辅助（避免依赖 strings 的额外 import）。
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
