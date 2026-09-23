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

	"github.com/kalandramo/TaiJi/internal/bootstrap"
)

// Options 控制一次 chat 会话。
type Options struct {
	Config      bootstrap.ModelConfig
	AppName     string
	UserID      string
	SessionID   string
	Instruction string
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
	ag := llmagent.New("assistant", agentOpts...)

	r := runner.NewRunner(opts.AppName, ag)
	defer r.Close()

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
		return bootstrap.WrapModelError(bootstrap.ResolveBaseURL(opts.Config), err)
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
		if ev.Response != nil && len(ev.Choices) > 0 {
			ch := ev.Choices[0]
			if delta := ch.Delta.Content; delta != "" {
				fmt.Fprint(opts.Out, delta)
				printed.WriteString(delta)
			} else if full := ch.Message.Content; full != "" && printed.Len() == 0 {
				fmt.Fprint(opts.Out, full)
				printed.WriteString(full)
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
