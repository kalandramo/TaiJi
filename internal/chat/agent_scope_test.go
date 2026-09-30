package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
)

// 多 agent 的 session 隔离（形态 C）。
//
// 每个 agent 一个 Executor，各自持独立 assembly（含独立 session service）。
// 本组测试锁住两条不变量：
//  1. AgentName 非空时，session 键加 agent 前缀（同 sessionID 的
//     不同 agent 历史互不可见）。
//  2. AgentName 为空时，session 键**不加前缀**（向后兼容——既有部署
//     升级后历史不失效）。

// captureMessages 起一个记录请求体 messages 的假模型端点。
func captureMessages(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var (
		mu   sync.Mutex
		last string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		b, _ := json.Marshal(req.Messages)
		mu.Lock()
		last = string(b)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\"," +
			"\"model\":\"m\",\"choices\":[{\"index\":0," +
			"\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}," +
			"\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// 同 sessionID 的两个 agent，历史必须隔离。
//
// **关键**：两个 agent 必须**共享**同一个 session service——
// 否则各自的 runner 自带独立 inmemory 存储，物理隔离会掩盖逻辑隔离，
// 本测试就变成空转（实测教训：首版用两个独立 Executor 时，
// 去掉 session 前缀反证**没有变红**，因为存储本来就不同）。
//
// 共享存储才让 agent 前缀成为**必需**：同 service 下键相同即同一会话。
func TestExecutor_AgentScopedSessionIsolation(t *testing.T) {
	srv, lastReq := captureMessages(t)

	// 共享一份 session 存储（模拟多 agent 部署）。
	shared := inmemory.NewSessionService()

	newExec := func(agent string) *Executor {
		ex, err := NewExecutor(Options{
			Config:         modelConfigFor(srv.URL),
			AppName:        "taiji",
			UserID:         "u",
			AgentName:      agent,
			SessionService: shared,
			Echo:           &strings.Builder{},
		})
		if err != nil {
			t.Fatalf("NewExecutor(%s): %v", agent, err)
		}
		t.Cleanup(ex.Close)
		return ex
	}

	exA := newExec("alpha")
	exB := newExec("beta")

	ctx := context.Background()
	// 同一 sessionID "s1" 分别在两个 agent 上跑两轮。
	if _, err := exA.Execute(ctx, "s1", "alpha-第一句"); err != nil {
		t.Fatalf("exA 第一句: %v", err)
	}
	if _, err := exB.Execute(ctx, "s1", "beta-第一句"); err != nil {
		t.Fatalf("exB 第一句: %v", err)
	}
	if _, err := exA.Execute(ctx, "s1", "alpha-第二句"); err != nil {
		t.Fatalf("exA 第二句: %v", err)
	}

	// 最后一次请求是 alpha 的第二轮——它应只看到 alpha 的历史
	// （alpha-第一句 + alpha-第二句），不含 beta 的任何内容。
	got := lastReq()
	if strings.Contains(got, "beta-第一句") {
		t.Errorf("agent 历史串话：alpha 的请求里出现了 beta 的内容。messages=%s", got)
	}
	if !strings.Contains(got, "alpha-第一句") {
		t.Errorf("alpha 的第二轮应带上一轮历史。messages=%s", got)
	}
}

// AgentName 为空时 session 键不加前缀（向后兼容）。
func TestExecutor_EmptyAgentNameKeepsLegacySessionKey(t *testing.T) {
	srv, lastReq := captureMessages(t)

	ex, err := NewExecutor(Options{
		Config:  modelConfigFor(srv.URL),
		AppName: "taiji",
		UserID:  "u",
		// AgentName 故意留空
		Echo: &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	defer ex.Close()

	ctx := context.Background()
	if _, err := ex.Execute(ctx, "s1", "第一句"); err != nil {
		t.Fatalf("第一句: %v", err)
	}
	if _, err := ex.Execute(ctx, "s1", "第二句"); err != nil {
		t.Fatalf("第二句: %v", err)
	}

	// 关键断言：同一 sessionID 的两轮仍共享历史（前缀未破坏既有行为）。
	got := lastReq()
	if !strings.Contains(got, "第一句") {
		t.Errorf("AgentName 为空时行为应与改动前一致（同 sessionID 共享历史），"+
			"但第二轮没看到第一轮。messages=%s", got)
	}
}

// scopedSessionID 的纯函数行为。
func TestScopedSessionID(t *testing.T) {
	if got := scopedSessionID("", "s1"); got != "s1" {
		t.Errorf("空 agent 名不应改 sessionID，got %q", got)
	}
	if got := scopedSessionID("alpha", "s1"); got == "s1" || !strings.Contains(got, "s1") {
		t.Errorf("非空 agent 名应加前缀且保留原 ID，got %q", got)
	}
	// 不同 agent 的同一 sessionID 必须不同。
	if scopedSessionID("alpha", "s1") == scopedSessionID("beta", "s1") {
		t.Error("不同 agent 的同一 sessionID 必须映射到不同键（否则历史串话）")
	}
}
