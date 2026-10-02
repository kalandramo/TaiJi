package server_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/kalandramo/TaiJi/internal/agentreg"
	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/cmd"
	"github.com/kalandramo/TaiJi/internal/server"
)

// 命令回复的 sender 必须与消息所属 agent 一致（2026-10-02 实测故障）。
//
// ## 现象（用户截图）
//
// 群里 @两个 bot 各发 /whoami，两条回复**都显示同一个 bot 的名字**：
//
//	@网络服务助手 /whoami → 回复发送者显示「道客服务助手」
//	@道客服务助手 /whoami → 回复发送者显示「道客服务助手」
//
// ## 根因
//
// 正常消息路径用 senderFor(agent) 选该 agent 的凭据（凭据隔离），
// 但命令路径的 replyCommand 用的是 p.sender（**全局单值**）——
// 多 agent 部署下命令回复总是由同一个 bot 发出。
//
// 这是「凭据隔离」在命令路径上的遗漏：普通回复隔离了，命令回复没有。

// labeledSender 把自己的 label 记入共享切片，从而能断言「谁发的」。
type labeledSender struct {
	label string
	mu    *sync.Mutex
	calls *[]string
}

func (s *labeledSender) SendMessage(_ context.Context, to, text string, _ channel.SendOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.calls = append(*s.calls, s.label)
	return "om_x", nil
}

func TestCommand_ReplyUsesAgentSpecificSender(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)
	mk := func(label string) channel.Sender {
		return &labeledSender{label: label, mu: &mu, calls: &calls}
	}

	registry := cmd.NewRegistry()
	cmd.RegisterBuiltins(registry, cmd.Deps{})

	exec := &echoExecutor{}
	agents, err := agentreg.New([]agentreg.Entry{
		{Agent: "alpha", AppID: "cli_a"},
		{Agent: "beta", AppID: "cli_b"},
	})
	if err != nil {
		t.Fatalf("agentreg.New: %v", err)
	}

	p, err := server.New(server.Config{
		Sender:   mk("DEFAULT"),
		Executor: exec,
		Gate: server.GateConfig{
			Activation: channel.ActivationAlways,
			Audience:   channel.AudienceEveryone,
		},
		Route:    channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap},
		Commands: registry,
		Agents:   agents,
		Executors: map[string]server.Executor{
			"alpha": exec,
			"beta":  exec,
		},
		Senders: map[string]channel.Sender{
			"alpha": mk("ALPHA"),
			"beta":  mk("BETA"),
		},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	// 消息属于 beta（app_id=cli_b）。
	msg := &channel.IncomingMessage{
		Platform:       channel.PlatformFeishu,
		AppID:          "cli_b",
		UserID:         "ou_x",
		UnionID:        "on_x",
		IdentitySource: channel.IdentitySourceUnionID,
		ChatID:         "oc_g",
		ChatType:       channel.ChatGroup,
		MessageID:      "om_1",
		Content:        "@_user_1 /whoami",
		Mentions:       []channel.Mention{{OpenID: "ou_bot_b", Key: "@_user_1"}},
	}
	if err := p.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("应有出站调用")
	}
	got := strings.Join(calls, ",")
	// 核心：必须用 beta 的凭据发出（凭据隔离在命令路径同样成立）。
	if !strings.Contains(got, "BETA") {
		t.Errorf("命令回复应由消息所属 agent（beta）的凭据发出，实际: %s", got)
	}
	if strings.Contains(got, "DEFAULT") {
		t.Errorf("命令回复不得用全局 sender（凭据隔离遗漏）: %s", got)
	}
	if strings.Contains(got, "ALPHA") {
		t.Errorf("命令回复不得用其它 agent 的凭据: %s", got)
	}
}
