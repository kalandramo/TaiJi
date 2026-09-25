package chat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/plugin"
	"trpc.group/trpc-go/trpc-agent-go/runner"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/bootstrap"
)

// 单轮执行 API（issue #9 Wave 2）。
//
// 为什么需要它：CLI 的 Run 是**交互循环**（bufio.Scanner + exit/quit），
// 服务端（飞书 webhook / 长连接）需要的是「给一句输入，拿完整回答」。
// 把「循环」与「单轮」拆开，两者复用同一套事件流语义——角色判定、
// 工具调用轮次、终止信号。否则两处各写一遍，语义必然漂移。
//
// 分工：
//   - chat.go 的 Run：CLI 循环，逐块打印（保留打字机效果）
//   - 本文件的 Executor：单轮，聚合完整回答（服务端一次性发送）
//   - 两者共用 runOneTurn 内核

// ErrBlankInput 表示输入为空或仅空白。
//
// 空输入在飞书侧会在门禁前被拦，但本 API 也要 fail-fast：
// 发起一次模型调用去问空问题既是浪费，也会让下游收到空回答后
// 往飞书发一条空消息。
var ErrBlankInput = errors.New("chat: input is blank")

// ErrMissingSessionID 表示调用方未提供会话标识。
//
// 服务端必须传会话级标识（如路由后的 effectiveJID）——漏传是装配缺陷，
// 显式失败优于静默让所有会话共用一份历史。
var ErrMissingSessionID = errors.New("chat: sessionID is required")

// Executor 持有长驻的 runner，供服务端跨消息复用。
//
// **为什么必须是长驻的**（实测发现的架构约束）：runner 持有 session service
// （runner.go:390 默认 inmemory）。每次新建 runner 就得到一份全新的
// session 存储——同一 SessionID 的历史无法延续，「第二条消息看到第一条
// 上下文」（AC-3）直接失效。
//
// 探针证据：两次独立装配各跑一轮 → 模型收到的消息数 [1 1]（历史丢失）；
// 同一个 runner 连跑两轮 → [1 1 1 3]（第二轮 3 条，历史保留）。
//
// 所以服务端必须在启动时装配一次 Executor，而不是每条消息调一次 Execute。
type Executor struct {
	asm *assembly
}

// NewExecutor 装配一个长驻执行器。服务端启动时调用一次。
func NewExecutor(opts Options) (*Executor, error) {
	asm, err := newRunner(opts)
	if err != nil {
		return nil, err
	}
	return &Executor{asm: asm}, nil
}

// Close 释放 runner 资源。服务端退出时调用。
func (e *Executor) Close() {
	if e != nil {
		e.asm.Close()
	}
}

// RegisteredTools 返回模型可见的工具名（排序后）。
//
// 用途：调用方在启动期打印「实际有哪些工具可放行」，避免配错名字后
// 只能从报错里反推。装配失败时拿不到（NewExecutor 返回 error），
// 故调用方需在装配**成功后**调用。
func (e *Executor) RegisteredTools() []string {
	if e == nil || e.asm == nil || e.asm.agent == nil {
		return nil
	}
	names := authz.ToolNames(e.asm.agent.Tools())
	sort.Strings(names)
	return names
}

// AllowedTools 返回生效的白名单（排序后）。
func (e *Executor) AllowedTools() []string {
	if e == nil || e.asm == nil || e.asm.policy == nil {
		return nil
	}
	return e.asm.policy.AllowedTools()
}

// Execute 执行单轮：发消息 → 消费事件流 → 返回完整回答。
//
// sessionID 决定历史归属：同一 sessionID 的多轮会带上彼此的历史，
// 不同 sessionID 互相隔离。调用方（服务端）应传**会话级**的稳定标识
// （如路由后的 effectiveJID），而不是每次生成新值——否则历史无法延续。
//
// 空 sessionID 返回错误而非补默认值：CLI 的 Run 会补 cli-<ts>（因为它
// 只有单一交互会话），但服务端漏传 sessionID 是**装配缺陷**——补默认值
// 会让所有会话共用一个历史（互相串话），且这个错误很难被发现。
// 实测教训：真实平台首次运行即报 "sessionID is required"（trpc 拒绝空值），
// 正是这条校验把它从「静默串话」变成了「显式失败」。
//
// Execute 执行单轮，返回完整回答。
//
// sessionID 决定历史归属：同一 sessionID 的多轮会带上彼此的历史，
// 不同 sessionID 互相隔离。调用方（服务端）应传**会话级**的稳定标识
// （如路由后的 effectiveJID），而不是每次生成新值——否则历史无法延续。
//
// 空 sessionID 返回错误而非补默认值：CLI 的 Run 会补 cli-<ts>（因为它
// 只有单一交互会话），但服务端漏传 sessionID 是**装配缺陷**——补默认值
// 会让所有会话共用一份历史，静默串话。
//
// ctx 取消会让本次执行提前结束并返回错误——服务端在进程退出时
// 依赖这条路径让在途请求收尾。
//
// 等价于 ExecuteStream(ctx, sessionID, input, nil)——保留该签名是因为
// 既有调用点（CLI 与多个测试）不需要流式，加参数会波及 8+ 处。
func (e *Executor) Execute(ctx context.Context, sessionID, input string) (string, error) {
	return e.ExecuteStream(ctx, sessionID, input, nil)
}

// ExecuteStream 执行单轮，逐块回调并返回完整回答（issue #10）。
//
// onChunk 非 nil 时，每个内容块到达即回调——调用方据此做流式展示
// （如更新飞书卡片）。onChunk 为 nil 时行为与 Execute 完全一致。
//
// **回调是同步的**：它在生成协程的调用栈内执行，慢回调会拖慢生成。
// 调用方若需异步（如网络请求），应自行加 buffer + goroutine——
// 本层不引入并发，因为那会让错误传递与生命周期管理复杂化。
//
// 回调里的 chunk 与返回的完整回答**拼接一致**：调用方可以只用 chunk
// 展示、用返回值做最终态，两者不会分叉。
func (e *Executor) ExecuteStream(ctx context.Context, sessionID, input string, onChunk func(string)) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", ErrMissingSessionID
	}
	text := strings.TrimSpace(input)
	if text == "" {
		return "", ErrBlankInput
	}

	var sb strings.Builder
	if err := runOneTurn(ctx, e.asm.runner, e.asm.opts, sessionID, text, func(chunk string) {
		sb.WriteString(chunk)
		if onChunk != nil {
			onChunk(chunk)
		}
	}); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// assembly 是一次装配的产物。
//
// 把 policy 与 agent 一起返回，是为了让「打印策略提示」不必重建 agent——
// 重建既浪费（要再走一遍工具注册）又会引入不一致（两次装配结果可能不同）。
type assembly struct {
	runner runner.Runner
	agent  *llmagent.LLMAgent
	policy *authz.ToolPolicy
	opts   Options
}

// newModel 构建模型 provider。
func newModel(opts Options) (model.Model, error) {
	return bootstrap.NewModel(opts.Config)
}

// newAgent 构建 agent。装配细节与 CLI 完全一致。
func newAgent(opts Options, m model.Model) (*llmagent.LLMAgent, error) {
	agentOpts := []llmagent.Option{
		llmagent.WithModel(m),
		// 必须显式开启流式：llmagent 默认走非流式（整段返回），
		// 那样「逐块输出」无从谈起（issue #2 AC-1）。
		// ForceNonStream 仅用于测试非流式回退路径。
		llmagent.WithGenerationConfig(model.GenerationConfig{Stream: !opts.ForceNonStream}),
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
	return llmagent.New("assistant", agentOpts...), nil
}

// newToolPolicy 构建工具策略（issue #4：默认拒绝 + 白名单放行）。
//
// 必须在 agent 建好之后装配——AC-4 的校验要用 agent 暴露的
// 「模型可见工具名」（MCP 工具带 {server}_ 前缀），而非原始 ToolSet 的裸名。
func newToolPolicy(opts Options, ag *llmagent.LLMAgent) (*authz.ToolPolicy, error) {
	return authz.BuildToolPolicy(authz.ToolPolicyConfig{
		Allow:      opts.AllowTools,
		Registered: ag.Tools(),
	})
}

// newRunner 装配一次运行所需的 runner 及关联组件。
//
// 抽出来是为了让 Run 与 Executor 用**完全相同**的装配路径——
// 模型、agent 选项、工具策略任何一处不同，CLI 与服务端的行为就会分叉，
// 而这类分叉很难在测试里被发现。
func newRunner(opts Options) (*assembly, error) {
	m, err := newModel(opts)
	if err != nil {
		return nil, err
	}
	ag, err := newAgent(opts, m)
	if err != nil {
		return nil, err
	}
	policy, err := newToolPolicy(opts, ag)
	if err != nil {
		return nil, err
	}

	// 插件按序串联，各管一个维度：
	//   approval         部署级——哪些工具在本部署启用（装配期定死）
	//   contextGuard     上下文级——来源是否允许写（IM 来源只读，§4.3.3）
	//   principalPolicy  用户级——谁能用哪些工具（读 ctx 里的 Principal）
	//
	// 三者都要：部署级防止"未启用的工具被调用"，上下文级防止"只读来源越权写"，
	// 用户级防止"越权使用"。顺序上部署级在前（纯内存查表、最便宜），
	// 上下文级居中（查 metadata 表），用户级最后（可能查外部数据源）。
	//
	// contextGuard 用 agent 暴露的「模型可见工具」构造 name→只读 表——
	// 只读性来自 MCP 的 readOnlyHint 注解（经 mcpTool.ToolMetadata 透传，
	// 已实测验证）。未声明注解的工具视为写操作（fail-closed）。
	plugins := []plugin.Plugin{
		policy.Plugin(),
		authz.NewContextGuardPlugin(ag.Tools(), opts.logf),
	}
	if opts.Permissions != nil {
		plugins = append(plugins, authz.NewPrincipalPolicyPlugin(opts.Permissions, opts.logf))
	}

	r := runner.NewRunner(opts.AppName, ag, runner.WithPlugins(plugins...))
	return &assembly{runner: r, agent: ag, policy: policy, opts: opts}, nil
}

// Close 释放 runner 资源。
func (a *assembly) Close() {
	if a != nil && a.runner != nil {
		a.runner.Close()
	}
}

// printToolPolicyHint 打印工具策略生效情况。
//
// CLI 打印是为了避免「配了工具却都不能用」的困惑。
func (a *assembly) printToolPolicyHint(opts Options) {
	if opts.Echo == nil {
		return
	}
	if len(opts.AllowTools) > 0 {
		fmt.Fprintf(opts.Echo, "工具策略：默认拒绝，放行 %v\n", a.policy.AllowedTools())
	} else if n := len(a.agent.Tools()); n > 0 {
		fmt.Fprintf(opts.Echo, "工具策略：默认拒绝，白名单为空——%d 个已注册工具均不可执行\n", n)
	}
}

// runOneTurn 执行一轮并把正文块交给 emit。
//
// 这是 Run 与 Executor 的共用内核：角色判定、非流式回退、终止信号
// 只在这里实现一次。emit 为 nil 表示只关心是否有输出（不逐块消费）。
//
// sessionID 由调用方传入而非从 opts 读——服务端需要 per-conversation
// 的会话键（路由结果），而 CLI 只有一个固定会话。
func runOneTurn(ctx context.Context, r runner.Runner, opts Options, sessionID, input string, emit func(string)) error {
	msg := model.NewUserMessage(input)

	events, err := r.Run(ctx, opts.UserID, sessionID, msg)
	if err != nil {
		// 此处 err 是 session/agent 选择类的同步错误，不含网络失败——
		// 模型连接错误走事件流（下方 ev.IsError()），其文本已由 provider
		// 注入请求 URL（实测：401 与连接拒绝均含完整 URL），故无需再包装。
		return err
	}

	printed := 0
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
		// 只取 assistant 角色的正文：工具调用链中，tool 角色的消息承载
		// 工具返回值，它是给模型看的中间产物，不是给用户的回答。若不区分
		// 角色，工具结果会被当作正文重复打印（issue #3 实测发现）。
		//
		// 只取 Choices[0]：未请求多候选（未设 GenerationConfig.N），
		// 所有主流 provider 默认 n=1。若将来启用多候选，此处需改为遍历
		// 并明确各候选的输出策略（否则其余候选会被静默丢弃）。
		if ev.Response != nil && len(ev.Choices) > 0 {
			ch := ev.Choices[0]
			if isAssistantText(ch.Delta.Role) {
				if delta := ch.Delta.Content; delta != "" {
					if emit != nil {
						emit(delta)
					}
					printed += len(delta)
				}
			}
			// 非流式回退：整段在 Message.Content，仅在尚未输出过时使用。
			if printed == 0 && isAssistantText(ch.Message.Role) {
				if full := ch.Message.Content; full != "" {
					if emit != nil {
						emit(full)
					}
					printed += len(full)
				}
			}
		}

		// v1.11.2 的终止信号是 runner completion（不是 IsFinalResponse）
		if ev.IsRunnerCompletion() {
			break
		}
	}

	if opts.Debug {
		fmt.Fprintf(opts.Echo, "\n[debug] 本轮输出 %d 字节\n", printed)
	}
	return nil
}
