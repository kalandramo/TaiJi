package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/feishu"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 日志隐私的端到端验收。
//
// 三条同时验证（一次真实消息流）：
//  1. 日志不含 open_id（隐私）
//  2. 日志含 message_id / chat_id（用户明确允许，且排障必需）
//  3. 脱敏后的主体标识可区分用户（审计可用）
//
// 为什么必须端到端：日志由管道（pipeline）产生，格式串与实参的
// 组合效果只有真实跑一遍才能确认——源码扫描证明不了"实际输出是什么"。

// capturingLogger 收集管道产生的全部日志（格式化后）。
type capturingLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *capturingLogger) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// 记录**格式化后**的输出——只记格式串看不到实参，验证会空转。
	l.msgs = append(l.msgs, fmt.Sprintf(format, args...))
}

func (l *capturingLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.msgs, "\n")
}

// 真实消息流 → 检查日志内容。
func TestLogPrivacy_EndToEnd(t *testing.T) {
	logger := &capturingLogger{}

	p, err := server.New(server.Config{
		Sender:   &recordingSender{},
		Executor: &echoExecutor{},
		Gate:     allowAllGate(),
		Route:    channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Logf:     logger.logf,
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

	// 群聊消息：open_id=ou_secret_user（eventJSON 的 sender），chat_id=oc_group_xyz
	mentions := `[{"key":"@_user_1","id":{"open_id":"ou_bot"},"name":"bot"}]`
	body := strings.Replace(
		eventJSON("om_msg_123", "oc_group_xyz", "group", "hi", mentions),
		"ou_sender", "ou_secret_user_openid", 1)

	req := httptest.NewRequest(http.MethodPost, "/webhook/feishu", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("webhook 返回 %d", w.Code)
	}

	// 等日志产生
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logger.joined(), "routed") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := logger.joined()
	if got == "" {
		t.Fatal("未产生任何日志")
	}
	t.Logf("实际日志：\n%s", got)

	// ── 验收1：不含 open_id ──
	if strings.Contains(got, "ou_secret_user_openid") {
		t.Errorf("日志含完整 open_id（隐私泄漏）:\n%s", got)
	}
	// ── 验收2：含 message_id / chat_id ──
	if !strings.Contains(got, "om_msg_123") {
		t.Errorf("日志应含 message_id（排障必需）:\n%s", got)
	}
	if !strings.Contains(got, "oc_group_xyz") {
		t.Errorf("日志应含 chat_id（排障必需）:\n%s", got)
	}
	// 验收3（脱敏标识可审计）不在此断言：
	// 本测试的 echoExecutor 是 fake，不经过真实工具调用链，故不产生
	// 权限插件的日志。该验收由 authz 层的运行时测试覆盖
	// （internal/authz/permission_redact_e2e_test.go，那里走真实插件）。
	//
	// 本测试只验证管道层日志：不含 open_id（验收1）+ 含 message_id/chat_id（验收2）。
}
