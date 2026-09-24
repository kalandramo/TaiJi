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

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/tool"

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

	// ForceNonStream 关闭流式，用于验证非流式回退路径
	// （整段内容在 Message.Content 而非 Delta.Content）。
	// 生产不应设置——原型默认走流式。
	ForceNonStream bool
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

	// 装配走 execute.go 的共用路径——CLI 与服务端必须用完全相同的
	// 模型/agent/工具策略装配，否则两者行为分叉且难以在测试中发现。
	asm, err := newRunner(opts)
	if err != nil {
		return err
	}
	defer asm.Close()

	asm.printToolPolicyHint(opts)

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

		if err := oneTurn(ctx, asm.runner, opts, line); err != nil { // 单轮失败不终止会话，打印后继续（除非是致命错误）
			fmt.Fprintf(opts.Echo, "\n[error] %v\n\n", err)
		}
		fmt.Fprintln(opts.Out)
	}
	return scanner.Err()
}

// oneTurn 执行一轮：发消息 → 消费事件流 → 逐块写入 Out。
//
// CLI 保留逐块输出（打字机效果）。服务端用 Executor.Execute（聚合完整回答）。
// 两者的差异只在 emit 回调，事件流语义共用 runOneTurn。
//
// CLI 用 opts.SessionID（Run 已确保非空——空则补 cli-<ts>）：
// CLI 只有一个交互会话，不像服务端需要 per-conversation 的键。
func oneTurn(ctx context.Context, r runner.Runner, opts Options, input string) error {
	return runOneTurn(ctx, r, opts, opts.SessionID, input, func(chunk string) {
		fmt.Fprint(opts.Out, chunk)
	})
}

// isAssistantText 判断该角色承载的是"给用户看的正文"。
//
// 放行 assistant 与空角色：多数 provider 的流式 delta 只在首块带 role，
// 后续块 role 为空（沿用前一块）。若把空角色判为"非正文"，会丢掉
// 首块之后的全部内容。tool 角色必须排除——它承载工具返回值。
func isAssistantText(role model.Role) bool {
	return role == "" || role == model.RoleAssistant
}
