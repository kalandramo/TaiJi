package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/feishu"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 身份贯通的端到端验证。
//
// 要证明的链路：
//   飞书消息（sender open_id）→ 门禁 → 管道注入 runCtx
//     → executor.Execute(ctx,...) 读到该 open_id
//
// 为什么必须端到端：注入点在 pipeline，消费点在下游。单元测试证明不了
// "注入的值真的到了消费端"——那是接线问题。
//
// 复用 e2e_test.go 的 eventJSON / allowAllGate / testToken / recordingSender，
// 只把 executor 换成会记录身份的版本。

// principalRecordingExecutor 记录 Execute 收到的 ctx 中的主体身份。
type principalRecordingExecutor struct {
	mu    sync.Mutex
	ids   []string
	hasID []bool
}

func (e *principalRecordingExecutor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	p, ok := authz.PrincipalFrom(ctx)
	e.mu.Lock()
	e.hasID = append(e.hasID, ok)
	if ok {
		e.ids = append(e.ids, p.ID)
	} else {
		e.ids = append(e.ids, "")
	}
	e.mu.Unlock()
	return "ok", nil
}

func (e *principalRecordingExecutor) snapshot() ([]string, []bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.ids...), append([]bool(nil), e.hasID...)
}

// newPrincipalHarness 构造用 principalRecordingExecutor 的夹具。
func newPrincipalHarness(t *testing.T) (*principalRecordingExecutor, func(string) int) {
	t.Helper()

	exec := &principalRecordingExecutor{}
	sender := &recordingSender{}

	p, err := server.New(server.Config{
		Sender:   sender,
		Executor: exec,
		Gate:     allowAllGate(),
		Route:    channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	d, err := server.NewDispatcher(server.DispatcherConfig{
		Handler: p,
		Deduper: channel.NewDeduper(time.Minute),
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	t.Cleanup(d.Stop)

	h := feishu.NewHandler(feishu.HandlerConfig{
		Verify:    feishu.VerifyConfig{VerificationToken: testToken},
		OnMessage: d.Enqueue,
	})

	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook/feishu", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	return exec, post
}

// waitExecutions 等执行次数达到 n。
func waitExecutions(exec *principalRecordingExecutor, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ids, _ := exec.snapshot(); len(ids) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// 私聊消息的 open_id 必须到达执行层。
func TestPrincipalE2E_SenderIDReachesExecutor(t *testing.T) {
	exec, post := newPrincipalHarness(t)

	// 私聊（chat_type=p2p）；eventJSON 里 sender open_id 是 "ou_sender"
	if code := post(eventJSON("om_principal_1", "", "p2p", "你好", "")); code != 200 {
		t.Fatalf("webhook 返回 %d, want 200", code)
	}
	waitExecutions(exec, 1, 3*time.Second)

	ids, hasID := exec.snapshot()
	if len(ids) == 0 {
		t.Fatal("执行器未被调用")
	}
	if !hasID[0] {
		t.Fatal("执行器未收到主体身份 —— 身份未贯通到执行层")
	}
	if !strings.Contains(ids[0], "ou_sender") {
		t.Errorf("主体 ID = %q，应含发送者 open_id \"ou_sender\"", ids[0])
	}
	t.Logf("✓ 执行层收到主体身份: %s", ids[0])
}

// 不同发送者 → 不同主体 ID（防"所有用户同一身份"）。
func TestPrincipalE2E_DifferentSendersDifferentIDs(t *testing.T) {
	exec, post := newPrincipalHarness(t)

	b1 := strings.Replace(eventJSON("om_p_a", "", "p2p", "hi", ""), "ou_sender", "ou_alice", 1)
	b2 := strings.Replace(eventJSON("om_p_b", "", "p2p", "hi", ""), "ou_sender", "ou_bob", 1)

	if post(b1) != 200 || post(b2) != 200 {
		t.Fatal("投递失败")
	}
	waitExecutions(exec, 2, 3*time.Second)

	ids, hasID := exec.snapshot()
	if len(ids) < 2 {
		t.Fatalf("只收到 %d 次执行，want 2", len(ids))
	}
	if !hasID[0] || !hasID[1] {
		t.Fatal("身份缺失")
	}
	if ids[0] == ids[1] {
		t.Errorf("两个不同发送者得到相同主体 ID = %q —— 身份未区分用户", ids[0])
	}
	t.Logf("✓ 两个发送者身份不同: %s / %s", ids[0], ids[1])
}

// 群聊消息的发送者身份同样要到达（群聊走 @ 判定路径，但身份不能丢）。
func TestPrincipalE2E_GroupMessageCarriesSender(t *testing.T) {
	exec, post := newPrincipalHarness(t)

	// 群聊 + @ bot（botOpenID 见 allowAllGate = "ou_bot"）
	mentions := `[{"key":"@_user_1","id":{"open_id":"ou_bot"},"name":"bot"}]`
	body := eventJSON("om_principal_g", "oc_group1", "group", "hi", mentions)
	if code := post(body); code != 200 {
		t.Fatalf("webhook 返回 %d, want 200", code)
	}
	waitExecutions(exec, 1, 3*time.Second)

	ids, hasID := exec.snapshot()
	if len(ids) == 0 {
		t.Fatal("群聊消息未触发执行")
	}
	if !hasID[0] {
		t.Fatal("群聊消息的身份未到达执行层")
	}
	if !strings.Contains(ids[0], "ou_sender") {
		t.Errorf("主体 ID = %q，应含发送者 open_id", ids[0])
	}
	t.Logf("✓ 群聊身份到达: %s", ids[0])
}
