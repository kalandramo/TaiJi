package server_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 日志隐私的端到端验收。
//
// 三条同时验证（一次真实消息流）：
//  1. 日志不含 open_id（隐私）
//  2. 日志含 message_id / chat_id（用户明确允许，且排障必需）
//
// **驱动方式已从 webhook 改为直投 Dispatcher**：原型只用长连接，
// webhook 实现已删除。长连接路径的真实形态是 SDK 回调产出
// IncomingMessage 后直接 Enqueue（longconn.go），故这里直接构造并投递。
//
// 为什么必须端到端：日志由管道（pipeline）产生，格式串与实参的组合效果
// 只有真实跑一遍才能确认——源码扫描证明不了"实际输出是什么"。
//
// 关于「脱敏标识可审计」：本测试的 echoExecutor 是 fake，不经过真实
// 工具调用链，故不产生权限插件的日志。该验收由 authz 层的运行时测试
// 覆盖（internal/authz/permission_redact_e2e_test.go，那里走真实插件）。

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

	// 群聊消息：open_id=ou_secret_user_openid（隐私，不应进日志）
	//           chat_id=oc_group_xyz（允许，排障必需）
	msg := &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    "ou_secret_user_openid",
		ChatID:    "oc_group_xyz",
		ChatType:  channel.ChatGroup,
		MessageID: "om_msg_123",
		Content:   "hi",
		Mentions:  []channel.Mention{{OpenID: "ou_bot", Key: "@_user_1", Name: "bot"}},
		Meta: &channel.ChannelMessageMeta{
			Provider:  string(channel.PlatformFeishu),
			ChatType:  "group",
			MessageID: "om_msg_123",
			Text:      "hi",
		},
	}
	if err := d.Enqueue(msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
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
}
