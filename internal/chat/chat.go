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
	//
	// 这是**部署级**策略：哪些工具在本部署启用。
	AllowTools []string
	// Permissions 是**用户级**权限源（方案 A：静态配置）。
	//
	// 与 AllowTools 串联，各管一个维度：
	//   AllowTools   部署级——工具是否在本部署启用（装配期校验）
	//   Permissions  用户级——该用户能否用这个工具（每次调用时判定）
	//
	// nil 表示不做用户级判定（向后兼容：CLI 等单用户场景）。
	// 非 nil 时，未授权用户的工具调用会被拒（fail-closed）。
	Permissions authz.PermissionSource
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

	// MaxToolIterations 是单次执行允许的**工具调用轮次上限**。
	//
	// 用途：给「确定性拒绝」加协议层兜底——模型收到权限拒绝后可能反复
	// 重试同一工具（实测见过连试 3 次），denyMessage 的「不要重试」只是
	// 对模型的行为指令，无法在协议层强制（见 authz/permission_plugin.go）。
	// 本上限是硬保障：轮次达上限即终止，不再发起 LLM 调用。
	//
	// 语义（对齐框架 llmagent.WithMaxToolIterations）：
	//   - > 0：生效，每轮 invocation 计数
	//   - <= 0：不限制（框架默认，保持既有行为）
	//
	// 计数与工具是否被执行**无关**（框架在权限检查前计数）——故被拒的
	// 重试也计入，这正是能挡住死循环的原因。
	MaxToolIterations int

	// ToolIterationFinalization 是轮次达上限时的**优雅终结指令**。
	//
	// 空串表示使用框架默认文案（calllimit.DefaultInstruction）。
	//
	// 为什么需要它：不配 finalization 时，超限会 emit 一个 flow_error
	// （"max tool iterations exceeded"），用户看到报错；配了则框架多做
	// 一次**无工具**的最终模型调用，把已有信息汇总成回答——用户体验是
	// 「得到答案」而非「报错」。
	ToolIterationFinalization string
}

// logf 把日志写到 Echo（未设置则丢弃）。
//
// 用途：用户级权限的拒绝必须留痕——否则"按用户管控"会在无声中失效。
func (o Options) logf(format string, args ...any) {
	if o.Echo == nil {
		return
	}
	fmt.Fprintf(o.Echo, "[perm] "+format+"\n", args...)
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

	// CLI 是交互式会话，是可写上下文（§4.3.3 的 ContextKind 表）。
	// 必须注入，否则上下文级守卫会把 CLI 的写操作 fail-closed 拒绝。
	// 仅在缺失时注入——尊重调用方已显式设置的 kind（便于测试）。
	if _, ok := authz.ContextKindFrom(ctx); !ok {
		ctx = authz.WithContextKind(ctx, authz.KindInteractive)
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
