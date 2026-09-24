package server_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 身份贯通的端到端验证。
//
// 要证明的链路：
//   平台消息（sender open_id）→ 门禁 → 管道注入 runCtx
//     → executor.Execute(ctx,...) 读到该 open_id
//
// **驱动方式已从 webhook 改为直投 Dispatcher**：原型只用长连接，
// webhook 实现已删除。长连接路径的真实形态是 SDK 回调产出
// IncomingMessage 后直接 Enqueue（longconn.go），故这里直接构造并投递。
//
// 为什么必须端到端：注入点在 pipeline，消费点在下游。单元测试证明不了
// "注入的值真的到了消费端"——那是接线问题。
//
// 复用 e2e_test.go 的 allowAllGate / recordingSender / contains。

// principalRecordingExecutor 记录 Execute 收到的 ctx 中的主体身份。
type principalRecordingExecutor struct {
	mu    sync.Mutex
	ids   []string
	hasID []bool
}

// ExecuteStream 满足 server.Executor 接口（issue #10）。
// 这些 fake 不测流式，故忽略 onChunk 直接委托 Execute。
func (e *principalRecordingExecutor) ExecuteStream(ctx context.Context, sessionID, input string, onChunk func(string)) (string, error) {
	return e.Execute(ctx, sessionID, input)
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
func newPrincipalHarness(t *testing.T) (*principalRecordingExecutor, *server.Dispatcher) {
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

	return exec, d
}

// msgWithSender 构造带指定发送者 open_id 的消息。
func msgWithSender(messageID, chatID, openID string, chatType channel.ChatType, mentions []channel.Mention) *channel.IncomingMessage {
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    openID,
		ChatID:    chatID,
		ChatType:  chatType,
		MessageID: messageID,
		Content:   "你好",
		Mentions:  mentions,
		Meta: &channel.ChannelMessageMeta{
			Provider:  string(channel.PlatformFeishu),
			ChatType:  string(chatType),
			MessageID: messageID,
			Text:      "你好",
		},
	}
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
	exec, d := newPrincipalHarness(t)

	if err := d.Enqueue(msgWithSender("om_principal_1", "", "ou_sender", channel.ChatDirect, nil)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitExecutions(exec, 1, 3*time.Second)

	ids, hasID := exec.snapshot()
	if len(ids) == 0 {
		t.Fatal("执行器未被调用")
	}
	if !hasID[0] {
		t.Fatal("执行器未收到主体身份 —— 身份未贯通到执行层")
	}
	if !contains(ids[0], "ou_sender") {
		t.Errorf("主体 ID = %q，应含发送者 open_id \"ou_sender\"", ids[0])
	}
	t.Logf("✓ 执行层收到主体身份: %s", ids[0])
}

// 不同发送者 → 不同主体 ID（防"所有用户同一身份"）。
func TestPrincipalE2E_DifferentSendersDifferentIDs(t *testing.T) {
	exec, d := newPrincipalHarness(t)

	if err := d.Enqueue(msgWithSender("om_p_a", "", "ou_alice", channel.ChatDirect, nil)); err != nil {
		t.Fatal(err)
	}
	if err := d.Enqueue(msgWithSender("om_p_b", "", "ou_bob", channel.ChatDirect, nil)); err != nil {
		t.Fatal(err)
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
	exec, d := newPrincipalHarness(t)

	mentions := []channel.Mention{{OpenID: "ou_bot", Key: "@_user_1", Name: "bot"}}
	msg := msgWithSender("om_principal_g", "oc_group1", "ou_sender", channel.ChatGroup, mentions)
	if err := d.Enqueue(msg); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitExecutions(exec, 1, 3*time.Second)

	ids, hasID := exec.snapshot()
	if len(ids) == 0 {
		t.Fatal("群聊消息未触发执行")
	}
	if !hasID[0] {
		t.Fatal("群聊消息的身份未到达执行层")
	}
	if !contains(ids[0], "ou_sender") {
		t.Errorf("主体 ID = %q，应含发送者 open_id", ids[0])
	}
	t.Logf("✓ 群聊身份到达: %s", ids[0])
}
