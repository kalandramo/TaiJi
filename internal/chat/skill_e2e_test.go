package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 端到端：Instruction 与 skill 是否真的到达模型请求。
//
// 这是本案最强的证据层——前面几层验证的是「配置被读取」（env 函数）、
// 「agent 装配有此选项」（unit test），本层验证「模型**真的收到**」。
//
// 为什么必须做这层：instruction 是装配期烘焙进 agent 的，若上游忽略了
// 该 Option，前面所有测试仍会全绿（它们只断言我们自己代码的行为）。
func TestE2E_InstructionReachesModelRequest(t *testing.T) {
	var (
		mu       sync.Mutex
		captured [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, _ := json.Marshal(req.Messages)
		mu.Lock()
		captured = append(captured, b)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		// 极简 SSE：一个 delta + 结束
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\","+
			"\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},"+
			"\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// 造 skill 目录
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: demo\ndescription: 端到端探针\n---\n正文"), 0o644); err != nil {
		t.Fatal(err)
	}

	const wantInstruction = "这是端到端测试的系统提示-UNIQUEMARKER"
	ex, err := NewExecutor(Options{
		Config:      modelConfigFor(srv.URL),
		AppName:     "e2e",
		UserID:      "u",
		SessionID:   "s1",
		Instruction: wantInstruction,
		SkillRoot:   root,
		Echo:        &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer ex.Close()

	t.Logf("RegisteredTools = %v", ex.RegisteredTools())

	if _, err := ex.Execute(context.Background(), "s1", "你好"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) == 0 {
		t.Fatal("模型端点未收到请求")
	}
	all := string(captured[0])
	if !strings.Contains(all, "UNIQUEMARKER") {
		t.Errorf("模型请求中未包含 Instruction 内容——"+
			"装配期的 WithInstruction 未生效。实际 messages: %s", all)
	} else {
		t.Logf("✓ Instruction 已到达模型（messages 含 UNIQUEMARKER）")
	}
}

// 回归护栏：SkillRoot 为空时，工具面与无 skill 时一致。
func TestE2E_NoSkillRootNoSkillTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
			`"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	ex, err := NewExecutor(Options{
		Config:  modelConfigFor(srv.URL),
		AppName: "e2e", UserID: "u",
		Echo: &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	defer ex.Close()

	for _, n := range ex.RegisteredTools() {
		if strings.HasPrefix(n, "skill_") || strings.HasPrefix(n, "workspace_") {
			t.Errorf("未配 SkillRoot 却注册了 %s——skill 应完全未启用", n)
		}
	}
	t.Log("✓ 未配 SkillRoot 时无 skill 工具（回归护栏）")
}
