// Package chat 实现 CLI 交互式对话循环（issue #2）。
//
// 链路：用户输入 → runner.Run → 模型流式响应 → 逐块打印。
// 多轮由 trpc-agent-go 的 session 承载：同一 sessionID 的多次 Run
// 自动带上历史（第二句的请求包含第一轮）。
package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/tool"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/bootstrap"
)

// Options 控制一次 chat 会话。
type Options struct {
	Config      bootstrap.ModelConfig
	AppName     string
	UserID      string
	SessionID   string
	Instruction string
	// ToolSets 是挂到 agent 上的工具集（如 MCP）。空表示无工具（issue #2 的行为）。
	// 工具名会由 llmagent 加上 {toolSetName}_ 前缀（issue #3 AC-2）。
	ToolSets []tool.ToolSet
	// AllowTools 是工具白名单（issue #4）。空表示全部拒绝（安全基线）。
	// 名字必须是「模型可见名」——MCP 工具要写 srvA_echo 而非 echo。
	// 装配期会校验名字是否已注册，未注册即报错（AC-4）。
	AllowTools []string
	// Out 接收模型输出（默认 stdout 由调用方传入）。
	Out io.Writer
	// Echo 接收提示与状态（默认 stderr）。
	Echo io.Writer
	// Debug 为真时打印每轮请求的消息条数（AC-3 多轮验证的观察点）。
	Debug bool
}

// Run 启动交互循环，直到 EOF 或用户输入 exit/quit。
func Run(ctx context.Context, in io.Reader, opts Options) error {
	if opts.Out == nil {
		return errors.New("chat: Out writer is required")
	}
	if opts.Echo == nil {
		opts.Echo = io.Discard
	}
	if opts.AppName == "" {
		opts.AppName = "taiji"
	}
	if opts.UserID == "" {
		opts.UserID = "local"
	}
	if opts.SessionID == "" {
		opts.SessionID = fmt.Sprintf("cli-%d", time.Now().Unix())
	}

	m, err := bootstrap.NewModel(opts.Config)
	if err != nil {
		return err
	}

	agentOpts := []llmagent.Option{
		llmagent.WithModel(m),
		// 必须显式开启流式：llmagent 默认走非流式（整段返回），
		// 那样 AC-1 的"逐块输出"无从谈起。
		llmagent.WithGenerationConfig(model.GenerationConfig{Stream: true}),
	}
	if s := strings.TrimSpace(opts.Instruction); s != "" {
		agentOpts = append(agentOpts, llmagent.WithInstruction(s))
	}
	if len(opts.ToolSets) > 0 {
		// 挂载工具集：llmagent 会用 NamedToolSet 包装，把工具名变成
		// {toolSetName}_{originalName}（见 trpc internal/tool/toolset.go:251），
		// 从而避免多个 MCP server 的同名工具冲突（issue #3 AC-2）。
		agentOpts = append(agentOpts, llmagent.WithToolSets(opts.ToolSets))
	}
	ag := llmagent.New("assistant", agentOpts...)

	// 工具策略（issue #4）：默认拒绝 + 白名单放行。
	// 必须在 agent 建好之后装配——AC-4 的校验要用 agent 暴露的
	// 「模型可见工具名」（MCP 工具带 {server}_ 前缀），而非原始 ToolSet 的裸名。
	policy, err := authz.BuildToolPolicy(authz.ToolPolicyConfig{
		Allow:      opts.AllowTools,
		Registered: ag.Tools(),
	})
	if err != nil {
		return err
	}

	runnerOpts := []runner.Option{runner.WithPlugins(policy.Plugin())}
	r := runner.NewRunner(opts.AppName, ag, runnerOpts...)
	defer r.Close()

	if len(opts.AllowTools) > 0 {
		fmt.Fprintf(opts.Echo, "工具策略：默认拒绝，放行 %v\n", policy.AllowedTools())
	} else if len(ag.Tools()) > 0 {
		// 有工具但无白名单：全部会被拒。显式提示，避免"配了工具却都不能用"的困惑。
		fmt.Fprintf(opts.Echo, "工具策略：默认拒绝，白名单为空——%d 个已注册工具均不可执行\n",
			len(ag.Tools()))
	}

	fmt.Fprintf(opts.Echo, "taiji chat · model=%s session=%s\n输入 exit 退出。\n\n",
		opts.Config.Name, opts.SessionID)

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		fmt.Fprint(opts.Echo, "> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}

		if err := oneTurn(ctx, r, opts, line); err != nil {
			// 单轮失败不终止会话，打印后继续（除非是致命错误）
			fmt.Fprintf(opts.Echo, "\n[error] %v\n\n", err)
		}
		fmt.Fprintln(opts.Out)
	}
	return scanner.Err()
}

// oneTurn 执行一轮：发消息 → 消费事件流 → 打印增量文本。
func oneTurn(ctx context.Context, r runner.Runner, opts Options, input string) error {
	msg := model.NewUserMessage(input)

	events, err := r.Run(ctx, opts.UserID, opts.SessionID, msg)
	if err != nil {
		// 此处 err 是 session/agent 选择类的同步错误，不含网络失败——
		// 模型连接错误走事件流（下方 ev.IsError()），其文本已由 provider
		// 注入请求 URL（实测：401 与连接拒绝均含完整 URL），故无需再包装。
		return err
	}

	var printed strings.Builder
	for ev := range events {
		if ev == nil {
			continue
		}
		if ev.IsError() {
			return fmt.Errorf("model error: %v", ev.Error)
		}

		// 增量文本：streaming 下内容在 Choices[0].Delta.Content。
		// 非流式回退：整段在 Choices[0].Message.Content（一次性输出）。
		//
		// 只打印 assistant 角色的正文：工具调用链中，tool 角色的消息承载
		// 工具返回值，它是给模型看的中间产物，不是给用户的回答。若不区分
		// 角色，工具结果会被当作正文重复打印（issue #3 实测发现）。
		//
		// 只取 Choices[0]：CLI 场景未请求多候选（未设 GenerationConfig.N），
		// 所有主流 provider 默认 n=1。若将来启用多候选，此处需改为遍历
		// 并明确各候选的输出策略（否则其余候选会被静默丢弃）。
		if ev.Response != nil && len(ev.Choices) > 0 {
			ch := ev.Choices[0]
			if isAssistantText(ch.Delta.Role) {
				if delta := ch.Delta.Content; delta != "" {
					fmt.Fprint(opts.Out, delta)
					printed.WriteString(delta)
				}
			}
			// 非流式回退：整段在 Message.Content，仅在尚未输出过时使用。
			if printed.Len() == 0 && isAssistantText(ch.Message.Role) {
				if full := ch.Message.Content; full != "" {
					fmt.Fprint(opts.Out, full)
					printed.WriteString(full)
				}
			}
		}

		// v1.11.2 的终止信号是 runner completion（不是 IsFinalResponse）
		if ev.IsRunnerCompletion() {
			break
		}
	}

	if opts.Debug {
		fmt.Fprintf(opts.Echo, "\n[debug] 本轮输出 %d 字节\n", printed.Len())
	}
	return nil
}

// isAssistantText 判断该角色承载的是"给用户看的正文"。
//
// 放行 assistant 与空角色：多数 provider 的流式 delta 只在首块带 role，
// 后续块 role 为空（沿用前一块）。若把空角色判为"非正文"，会丢掉
// 首块之后的全部内容。tool 角色必须排除——它承载工具返回值。
func isAssistantText(role model.Role) bool {
	return role == "" || role == model.RoleAssistant
}
