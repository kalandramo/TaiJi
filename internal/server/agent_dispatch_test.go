package server

import (
	"context"
	"errors"
	"testing"

	"github.com/kalandramo/TaiJi/internal/agentreg"
	"github.com/kalandramo/TaiJi/internal/channel"
)

// 多 agent 分流（形态 C：Bot 即 agent）的端到端测试。
//
// 不变量：
//  1. 按消息的 AppID 分流到对应 agent 的 executor（不是别的 agent 的）。
//  2. 出站用对应 agent 的 sender（**凭据隔离的落点**——回复来自正确的 bot）。
//  3. 未知 AppID fail-closed（不落到默认 agent）。
//  4. 未启用多 agent（Agents==nil）时行为与改动前完全一致。

// mkDispatchPipeline 构造一个启用了多 agent 分流的管道。
func mkDispatchPipeline(t *testing.T, reg *agentreg.Registry,
	execs map[string]Executor, senders map[string]channel.Sender) *Pipeline {

	t.Helper()
	pl, err := New(Config{
		Sender:    senders["default"],
		Executor:  execs["default"],
		Agents:    reg,
		Executors: execs,
		Senders:   senders,
		Gate: GateConfig{
			Audience:   channel.AudienceEveryone,
			Activation: channel.ActivationAlways,
		},
		Route:       channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingSingleContext},
		Placeholder: "思考中…",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return pl
}

func inboxMsg(appID, content string) *channel.IncomingMessage {
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		AppID:     appID,
		UserID:    "ou_sender",
		ChatID:    "oc_group",
		ChatType:  channel.ChatGroup,
		MessageID: "om_" + appID,
		Content:   content,
	}
}

func TestDispatch_RoutesToMatchingAgent(t *testing.T) {
	reg, err := agentreg.New([]agentreg.Entry{
		{AppID: "cli_aaa", Agent: "alpha"},
		{AppID: "cli_bbb", Agent: "beta"},
	})
	if err != nil {
		t.Fatalf("agentreg.New: %v", err)
	}

	exA, exB := &fakeExecutor{}, &fakeExecutor{}
	exDefault := &fakeExecutor{}
	p := mkDispatchPipeline(t, reg,
		map[string]Executor{"alpha": exA, "beta": exB, "default": exDefault},
		map[string]channel.Sender{
			"alpha": &fakeSender{}, "beta": &fakeSender{}, "default": &fakeSender{},
		})

	// alpha 的 bot 收到消息 → 应落到 exA。
	if err := p.Handle(context.Background(), inboxMsg("cli_aaa", "问题A")); err != nil {
		t.Fatalf("Handle(cli_aaa): %v", err)
	}
	if exA.runCount() != 1 {
		t.Errorf("cli_aaa 的消息应由 alpha 的 executor 执行，实际 alpha=%d", exA.runCount())
	}
	if exB.runCount() != 0 || exDefault.runCount() != 0 {
		t.Errorf("不应落到其他 agent：beta=%d default=%d", exB.runCount(), exDefault.runCount())
	}

	// beta 的 bot 收到消息 → 应落到 exB。
	if err := p.Handle(context.Background(), inboxMsg("cli_bbb", "问题B")); err != nil {
		t.Fatalf("Handle(cli_bbb): %v", err)
	}
	if exB.runCount() != 1 {
		t.Errorf("cli_bbb 的消息应由 beta 的 executor 执行，实际 beta=%d", exB.runCount())
	}
}

// **凭据隔离**：回复必须走对应 agent 的 sender（否则用户收到
// 「来自错误 bot」的消息）。
func TestDispatch_RepliesViaMatchingSender(t *testing.T) {
	reg, _ := agentreg.New([]agentreg.Entry{{AppID: "cli_aaa", Agent: "alpha"}})

	exA := &fakeExecutor{}
	sA := &fakeSender{}
	sDefault := &fakeSender{}
	p := mkDispatchPipeline(t, reg,
		map[string]Executor{"alpha": exA, "default": &fakeExecutor{}},
		map[string]channel.Sender{"alpha": sA, "default": sDefault})

	if err := p.Handle(context.Background(), inboxMsg("cli_aaa", "你好")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if sA.count() == 0 {
		t.Error("alpha 的回复必须经 alpha 的 sender 发出（凭据隔离）")
	}
	if sDefault.count() != 0 {
		t.Errorf("不得用默认 sender 发 alpha 的回复，实际 default 发了 %d 条", sDefault.count())
	}
}

// 未知 AppID fail-closed：不落到任何 agent（含默认）。
func TestDispatch_UnknownAppIDFailsClosed(t *testing.T) {
	reg, _ := agentreg.New([]agentreg.Entry{{AppID: "cli_aaa", Agent: "alpha"}})

	exA, exDefault := &fakeExecutor{}, &fakeExecutor{}
	p := mkDispatchPipeline(t, reg,
		map[string]Executor{"alpha": exA, "default": exDefault},
		map[string]channel.Sender{"alpha": &fakeSender{}, "default": &fakeSender{}})

	// 未知 app_id **不返回错误**（入站消息静默丢弃，与门禁拒绝同取向），
	// 但**必须不执行**任何 agent。
	if err := p.Handle(context.Background(), inboxMsg("cli_unknown", "你好")); err != nil {
		t.Fatalf("未知 app_id 应静默丢弃而非报错: %v", err)
	}
	if exA.runCount() != 0 || exDefault.runCount() != 0 {
		t.Errorf("未知 app_id 不得落到任何 agent（含默认）：alpha=%d default=%d",
			exA.runCount(), exDefault.runCount())
	}
}

// 回归护栏：未启用多 agent 时，行为与改动前完全一致。
func TestDispatch_NoRegistryKeepsLegacyBehaviour(t *testing.T) {
	ex := &fakeExecutor{}
	sd := &fakeSender{}
	p, err := New(Config{
		Sender:   sd,
		Executor: ex,
		// Agents 故意为 nil（未启用多 agent）
		Gate: GateConfig{
			Audience:   channel.AudienceEveryone,
			Activation: channel.ActivationAlways,
		},
		Route:       channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingSingleContext},
		Placeholder: "思考中…",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	msg := inboxMsg("cli_whatever", "你好")
	if err := p.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if ex.runCount() != 1 {
		t.Errorf("未启用多 agent 时应走单值 executor，实际 %d 次", ex.runCount())
	}
	if sd.count() == 0 {
		t.Error("未启用多 agent 时应走单值 sender")
	}
}

// 纯函数：注册表条目到 Executors/Senders 的装配校验。
func TestDispatch_MissingExecutorIsError(t *testing.T) {
	reg, _ := agentreg.New([]agentreg.Entry{{AppID: "cli_aaa", Agent: "alpha"}})
	p := mkDispatchPipeline(t, reg,
		map[string]Executor{"default": &fakeExecutor{}}, // 缺 alpha
		map[string]channel.Sender{"default": &fakeSender{}, "alpha": &fakeSender{}})

	err := p.Handle(context.Background(), inboxMsg("cli_aaa", "你好"))
	if err == nil {
		t.Fatal("agent 无对应执行器应报错（装配缺陷），不能静默")
	}
}

// 纯函数：resolveAgent 的行为。
func TestResolveAgent(t *testing.T) {
	reg, _ := agentreg.New([]agentreg.Entry{{AppID: "cli_aaa", Agent: "alpha"}})
	p := &Pipeline{agents: reg}

	got, err := p.resolveAgent(inboxMsg("cli_aaa", "x"))
	if err != nil || got != "alpha" {
		t.Errorf("resolveAgent(cli_aaa) = (%q,%v), want (alpha,nil)", got, err)
	}

	_, err = p.resolveAgent(inboxMsg("cli_unknown", "x"))
	if !errors.Is(err, agentreg.ErrUnknownApp) {
		t.Errorf("未知 app_id 应返回 ErrUnknownApp，got %v", err)
	}

	// nil 注册表 → 空 agent（走单值路径）。
	p2 := &Pipeline{}
	got2, err2 := p2.resolveAgent(inboxMsg("cli_x", "x"))
	if err2 != nil || got2 != "" {
		t.Errorf("未启用多 agent 应返回空 agent，got (%q,%v)", got2, err2)
	}
}
