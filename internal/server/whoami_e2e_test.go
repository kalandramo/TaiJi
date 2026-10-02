package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/cmd"
	"github.com/kalandramo/TaiJi/internal/server"
)

// /whoami 的端到端行为（走完整 dispatcher → pipeline 链路）。
//
// 为什么需要它（而非只测 whoamiText 纯函数）：纯函数正确但**没接上管道**
// 等于没修——这类接线缺口只有走完整调用链才抓得到。本会话已多次遇到
// 「单元测试绿、接线是断的」（如 buildAgents 曾漏赋字段导致分流静默失效）。
//
// 用户级验收：发 /whoami → 收到含**身份键**的回复，且模型未被调用。

// newWhoamiHarness 构造带命令系统的夹具（复用既有 fake）。
//
// 与 newHarness 的差异只有一处：注入 Commands（nil 表示禁用命令，
// 既有夹具保持不动是刻意的向后兼容——见 server.Config.Commands 注释）。
func newWhoamiHarness(t *testing.T) (*harness, *recordingSender, *echoExecutor) {
	t.Helper()
	sender := &recordingSender{}
	exec := &echoExecutor{}

	registry := cmd.NewRegistry()
	cmd.RegisterBuiltins(registry, cmd.Deps{})

	p, err := server.New(server.Config{
		Sender:   sender,
		Executor: exec,
		Gate: server.GateConfig{
			Activation: channel.ActivationAlways,
			Audience:   channel.AudienceEveryone,
		},
		Route:    channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Commands: registry,
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
	return &harness{dispatcher: d, sender: sender, executor: exec}, sender, exec
}

func TestE2E_WhoamiRepliesWithIdentityKeyAndSkipsModel(t *testing.T) {
	h, sender, exec := newWhoamiHarness(t)

	// 消息带 union_id（身份键来源）——模拟真实长连接事件的产物。
	h.send(t, &channel.IncomingMessage{
		Platform:       channel.PlatformFeishu,
		UserID:         "ou_open_in_app",  // 应用维度的 open_id
		UnionID:        "on_stable_union", // 跨应用稳定的身份键
		IdentitySource: channel.IdentitySourceUnionID,
		ChatType:       channel.ChatDirect,
		MessageID:      "om_whoami_1",
		Content:        "/whoami",
	})

	if !h.waitCalls(t, 1, 2*time.Second) {
		t.Fatal("发 /whoami 应有出站回复，却超时未收到")
	}

	calls := sender.snapshot()
	got := calls[0].Text

	// ① 核心：回复含**身份键**（union_id 派生），而非 open_id。
	if !strings.Contains(got, "on_stable_union") {
		t.Errorf("回复应含身份键（union_id 派生），实际:\n%s", got)
	}
	// ② 形态必须是可直接粘进 RBAC 的完整主体 ID。
	if !strings.Contains(got, "ws1:feishu:on_stable_union") {
		t.Errorf("回复应含可直接配置的主体 ID（{ws}:{platform}:{identity}），实际:\n%s", got)
	}
	// ③ 必须提示配置位置（否则用户拿到 ID 不知写哪）。
	if !strings.Contains(got, "TAIJI_RBAC") {
		t.Errorf("回复应提示配置位置，实际:\n%s", got)
	}
	// ④ 命令不走模型——executor 不得被调用。
	if inputs := exec.inputs(); len(inputs) != 0 {
		t.Errorf("命令不应调用模型，executor 却被调用: %v", inputs)
	}
}

// 反证：把身份键换回 open_id（模拟未修复），端到端测试必须变红。
//
// 这条不是「多测一遍」——它锁住的是「修复确实经由管道生效」。
// 若哪天有人把 pipeline 的 msg.IdentityID() 改回 msg.UserID，
// 这个测试会红。
func TestE2E_WhoamiUsesUnionIDNotOpenID(t *testing.T) {
	h, sender, _ := newWhoamiHarness(t)

	// 故意让 open_id 与 union_id 都能被识别，断言输出用的是后者。
	h.send(t, &channel.IncomingMessage{
		Platform:       channel.PlatformFeishu,
		UserID:         "ou_MUST_NOT_APPEAR",
		UnionID:        "on_SHOULD_APPEAR",
		IdentitySource: channel.IdentitySourceUnionID,
		ChatType:       channel.ChatDirect,
		MessageID:      "om_whoami_2",
		Content:        "/whoami",
	})

	if !h.waitCalls(t, 1, 2*time.Second) {
		t.Fatal("应收到回复")
	}
	got := sender.snapshot()[0].Text

	if !strings.Contains(got, "on_SHOULD_APPEAR") {
		t.Errorf("主体 ID 应基于 union_id，实际:\n%s", got)
	}
	// open_id 会被单独列出（用于排查「哪个应用」），但**不能**出现在
	// 主体 ID 的位置。判据：主体 ID 行不得含 open_id。
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "主体 ID") && strings.Contains(line, "ou_MUST_NOT_APPEAR") {
			t.Errorf("主体 ID 不得基于 open_id（应用维度，跨应用会不一致）:\n%s", line)
		}
	}
}

// 反证（安全附录）：豁免**不得**扩散到其它命令。
//
// /whoami 免权限是「循环依赖」的**特例**，不是权限模型的放宽。
// 若哪天有人把 NoPermission 误加到 /status，这个测试必须变红——
// 它锁住的是「豁免是精确的、可审计的」。
func TestE2E_NoPermissionBypassDoesNotLeakToOtherCommands(t *testing.T) {
	h, sender, _ := newWhoamiHarness(t)

	// /status 未标 NoPermission，且本夹具**未配权限源**——
	// 故它必须被拒（fail-closed）。
	h.send(t, &channel.IncomingMessage{
		Platform:       channel.PlatformFeishu,
		UserID:         "ou_x",
		UnionID:        "on_x",
		IdentitySource: channel.IdentitySourceUnionID,
		ChatType:       channel.ChatDirect,
		MessageID:      "om_status_1",
		Content:        "/status",
	})

	if !h.waitCalls(t, 1, 2*time.Second) {
		t.Fatal("应收到回复（拒绝也是回复）")
	}
	got := sender.snapshot()[0].Text

	if strings.Contains(got, "会话状态") {
		t.Errorf("/status 未标 NoPermission，不应被放行:\n%s", got)
	}
	if !strings.Contains(got, "权限") {
		t.Errorf("/status 应被权限拒绝，实际:\n%s", got)
	}
}

// 反证：NoPermission 只跳过 RBAC，**不跳过** OwnerOnly。
//
// 两者正交（registry.go 的 Command 注释）——豁免一个不该连带另一个。
// 若有人把 /clear 同时设上 NoPermission（错误做法），owner 判定仍须生效。
func TestE2E_NoPermissionDoesNotBypassOwnerOnly(t *testing.T) {
	r := cmd.NewRegistry()
	cmd.RegisterBuiltins(r, cmd.Deps{})

	c, ok := r.Lookup("clear")
	if !ok {
		t.Fatal("/clear 未注册")
	}
	if !c.OwnerOnly {
		t.Error("/clear 应保持 OwnerOnly")
	}
	if c.NoPermission {
		t.Error("/clear 不应有 NoPermission——它会改状态（换 session）")
	}

	// /whoami 则相反：免权限、不限 owner。
	w, _ := r.Lookup("whoami")
	if !w.NoPermission {
		t.Error("/whoami 应有 NoPermission")
	}
	if w.OwnerOnly {
		t.Error("/whoami 不应限 owner——无权用户更需要自查身份")
	}
}
