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

	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
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
	// **范围取决于装配方**（2026-10-02 修正，原注释写「部署级」不准确）：
	//   - 单 agent：部署级——全局一份
	//   - 多 agent：可由 `TAIJI_AGENTS` 的 `allow_tools=` 按 agent 覆盖
	//     （见 cmd/taiji 的 buildOne：spec 非空则用它，否则回退全局）
	//
	// 每 agent 各建一个策略插件（newToolPolicy 用该 agent 自己的 Tools()），
	// 故 per-agent 白名单是真实生效的，不只是配置面写法。
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

	// SkillRoot 是 skill 仓库根目录（可含多个，用 os.PathListSeparator 分隔）。
	//
	// 格式约定同上游 skill.FSRepository：每个子目录含一个 SKILL.md，
	// 带可选 YAML front matter（name/description）。缺 name 时回落为目录名。
	//
	// 空串表示不启用 skill——此时装配路径与既有行为**完全一致**（回归护栏）。
	//
	// 为什么收路径而非 skill.Repository：本包不 import skill 包，保持
	// 「不依赖具体能力实现」的定位（与 cmd 包不 import authz 同构）。
	// 仓库构造与失败处理在装配期本包内完成。
	SkillRoot string

	// AgentName 是本次装配服务的 agent 标识（多 agent 部署用）。
	//
	// 作用：决定 session 键是否加 agent 前缀——不同 agent 的历史必须隔离
	// （同一用户在 agent A 的对话不应出现在 agent B 的上下文里）。
	//
	// 空串表示单 agent 部署，session 键**保持原样**（向后兼容：既有部署
	// 升级后历史不失效）。这比「恒加前缀」更保守——后者会让所有现有
	// 会话历史在升级瞬间失联。
	AgentName string

	// SessionService 是**可选的共享** session 存储（多 agent 部署用）。
	//
	// nil 时每个 Executor 各建一份 inmemory（默认，等价单 agent 行为）。
	// 非 nil 时多个 agent 共享同一存储——省内存，但**必须**配合
	// AgentName（session 键加 agent 前缀），否则同名会话会串话。
	//
	// 为什么提供共享选项：多 agent 部署若各持一份 session 存储，
	// 内存随 agent 数线性增长，而多数部署的历史互访需求有限。
	SessionService session.Service

	// SkillToolProfile 控制启用哪些上游 skill 工具。
	//
	// 空串 → 用上游默认（KnowledgeOnly：只注册 skill_load /
	// skill_list_docs / skill_select_docs，不含执行类工具）。
	//
	// 为什么默认 KnowledgeOnly 而非 Full：Full 会注册 skill_run /
	// skill_exec 等**代码执行**工具。本项目 serve 面向 IM 用户，
	// 且 IM 来源是只读上下文（§4.3.3）——执行工具的语义与此冲突。
	// 需要执行能力时应显式配置，并自行评估沙箱。
	SkillToolProfile string

	// SubAgents 是本 agent 的**子 agent 列表**（N1 父子关系）。
	//
	// 空表示叶子 agent。非空时上游会给本 agent 的 Tools() 追加
	// **一个** transfer_to_agent 工具（不是每子一个），模型在其中
	// 通过 agent_name 选目标（外部依赖 trpc-agent-go 的
	// agent/llmagent/llm_agent.go 的 getAllToolsLocked）。
	//
	// **名字必须唯一**：上游的 FindSubAgent(name) 按名查找，
	// 重名会有歧义——这正是 AgentName 必须真正生效的原因（见 newAgent）。
	SubAgents []agent.Agent
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
