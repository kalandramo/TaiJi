// Command taiji 是 taiji-proto 的入口。
//
// 两种模式：
//
//	taiji chat  —— CLI 交互式对话（验收线 1、2）
//	taiji serve —— 渠道服务（webhook / 长连接，验收线 3、4）
//
// 本 Issue（#1）只建立骨架：子命令存在、--help 可用、配置加载打通。
// chat/serve 的实际行为由后续 Issue 填充。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/bootstrap"
	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/feishu"
	"github.com/kalandramo/TaiJi/internal/chat"
	"github.com/kalandramo/TaiJi/internal/config"
	"github.com/kalandramo/TaiJi/internal/server"
)

// signalContext 返回在收到中断信号时取消的 context，
// 让长连接/流式请求能优雅退出（issue #9 会用到同样的路径）。
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

const usage = `taiji — Go 原生 Agent Harness（原型）

用法:
  taiji <command> [flags]

命令:
  chat     交互式对话
  serve    启动渠道服务（飞书 webhook / 长连接）

全局 flags:
  --config <path>   工作区环境文件（不能覆盖保留键，见设计文档 §4.6）
  -h, --help        显示帮助

示例:
  taiji chat
  taiji serve --config configs/taiji.example.conf
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "chat":
		return runChat(args[1:])
	case "serve":
		return runServe(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "taiji: unknown command %q\n\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
}

// newFlagSet 为每个子命令建独立 flag 集，避免全局状态串扰。
func newFlagSet(name, desc string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfg := fs.String("config", "", "工作区环境文件路径（可选）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s — %s\n\n用法: taiji %s [flags]\n\nflags:\n", name, desc, name)
		fs.PrintDefaults()
	}
	return fs, cfg
}

// loadConfig 加载配置并打印生效值，是 #1 的 demo path 载体。
func loadConfig(path string) (map[string]string, error) {
	warn := func(msg string) {
		fmt.Fprintf(os.Stderr, "[warn] %s\n", msg)
	}
	return config.Load(config.SnapshotEnv(), path, warn)
}

func runChat(args []string) int {
	fs, cfg := newFlagSet("chat", "交互式对话")
	instruction := fs.String("instruction", "", "系统提示（可选）")
	debug := fs.Bool("debug", false, "打印每轮调试信息（含多轮历史观察点）")
	mcpSpecs := multiFlag{}
	fs.Var(&mcpSpecs, "mcp", "挂载 MCP server，格式 name=command [args...]（stdio）或 name=http(s)://host/path（远程，可重复）")
	allowTools := multiFlag{}
	fs.Var(&allowTools, "allow-tool", "放行的工具名（模型可见名，如 srvA_echo；可重复）。未列出的工具一律拒绝")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	loaded, err := loadConfig(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji chat: %v\n", err)
		return 1
	}
	// 打印控制值生效情况（#1 demo path：证明工作区未能覆盖保留键）
	printControlValues(loaded)

	// 模型配置来自受信启动环境（设计文档 §4.6）
	modelCfg := bootstrap.ModelConfigFromEnv()
	if v, ok := loaded[bootstrap.EnvModelName]; ok && v != "" {
		modelCfg.Name = v
	}
	if v, ok := loaded[bootstrap.EnvModelBaseURL]; ok {
		modelCfg.BaseURL = v
	}

	// MCP 装配：配置非法或 server 起不来 → 立即失败退出（issue #3 AC-3）
	//
	// 来源合并：环境变量（TAIJI_MCP_SERVERS）在前，--mcp flag 追加在后。
	// 两条路径都认，因为 CLI 与服务端（serve）应共用同一套配置面——
	// 只认 flag 会让「设了环境变量却在 CLI 里不生效」成为静默缺口。
	mcpCfgs, err := parseMCPSpecs(append(envMCPSpecs(), mcpSpecs...))
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji chat: %v\n", err)
		return 2
	}
	toolSets, err := bootstrap.NewMCPSets(mcpCfgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji chat: %v\n", err)
		return 1
	}
	defer bootstrap.CloseMCPSets(toolSets)
	for _, c := range mcpCfgs {
		fmt.Fprintf(os.Stderr, "[mcp] %s 已就绪\n", c.Name)
	}

	ctx, cancel := signalContext()
	defer cancel()

	// CLI 不挂用户级权限插件（issue #6 缺口 3 的镜像修复）。
	//
	// 理由：用户级权限按 IM 主体（open_id）判定，而 CLI 是本地单用户、
	// 无 IM 身份——PrincipalPolicyPlugin 对「无 Principal」拒绝，挂上它
	// 会让 CLI 的所有工具调用被拒（实测确认的静默失效）。
	// Options.Permissions 的注释本就写明 CLI 属「nil」场景。
	//
	// 若用户设了任一权限变量（误以为对 CLI 生效），显式提示而非静默忽略——
	// 否则「配了却不生效」又是一个静默缺口。
	if envRBAC() != nil || envPermissions() != nil {
		fmt.Fprintf(os.Stderr,
			"taiji chat: 注意——用户级权限配置（TAIJI_RBAC / TAIJI_USER_PERMISSIONS）"+
				"对 CLI 无效（权限按 IM 主体判定，CLI 无 IM 身份）。该配置仅 serve 生效。\n")
	}

	err = chat.Run(ctx, os.Stdin, chat.Options{
		Config:      modelCfg,
		Instruction: *instruction,
		ToolSets:    toolSets,
		AllowTools:  append(envAllowTools(), allowTools...),
		// Permissions 刻意不传：CLI 无 IM 身份，用户级权限不适用。
		// 工具调用轮次上限：给确定性拒绝加协议层兜底（缺口 4）。
		MaxToolIterations: envMaxToolIterations(),
		Out:               os.Stdout,
		Echo:              os.Stderr,
		Debug:             *debug,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji chat: %v\n", err)
		return 1
	}
	return 0
}

// multiFlag 收集可重复的字符串 flag。
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// mcpHeadersPrefix 是 MCP 认证头的环境变量前缀。
//
// 格式：TAIJI_MCP_HEADERS_<SERVER名>=Header1:Value1;Header2:Value2
// 例：  TAIJI_MCP_HEADERS_github=Authorization:Bearer ghp_xxx
//
// 为什么走环境变量而非 --config 工作区文件：这些头含 token/API key，属凭据。
// 工作区是 agent 可写区域，落在那里的凭据可被改写（凭据劫持）。
// config.ReservedPrefixes 锁住该前缀，工作区提供的同名键会被跳过。
const mcpHeadersPrefix = "TAIJI_MCP_HEADERS_"

// parseMCPSpecs 把 MCP server 配置解析成 MCPServerConfig。
//
// 支持的形态：
//
//	name=command [args...]            → stdio（本地子进程，无需认证）
//	name=http://host/mcp              → streamable（远程，可带认证头）
//	name=https://host/sse             → sse（远程，可带认证头）
//
// 远程形态的认证头从 TAIJI_MCP_HEADERS_<name> 读取（见 mcpHeadersPrefix）。
func parseMCPSpecs(specs []string) ([]bootstrap.MCPServerConfig, error) {
	out := make([]bootstrap.MCPServerConfig, 0, len(specs))
	for _, s := range specs {
		name, rest, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("invalid MCP spec %q: expected name=command [args...] or name=url", s)
		}
		name = strings.TrimSpace(name)
		rest = strings.TrimSpace(rest)

		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			cfg := bootstrap.MCPServerConfig{
				Name:      name,
				Transport: mcpTransportForURL(rest),
				URL:       rest,
				Headers:   mcpHeadersFor(name),
			}
			out = append(out, cfg)
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil, fmt.Errorf("invalid MCP spec %q: command is empty", s)
		}
		out = append(out, bootstrap.MCPServerConfig{
			Name:      name,
			Transport: "stdio",
			Command:   fields[0],
			Args:      fields[1:],
		})
	}
	return out, nil
}

// mcpTransportForURL 按 URL 形态推断 transport。
//
// 依据：MCP 的 SSE 端点约定以 /sse 结尾；其余按 streamable HTTP
// （streamable 是 MCP 2025 规范推荐形态，作为默认更安全）。
func mcpTransportForURL(u string) string {
	if strings.HasSuffix(strings.TrimSuffix(u, "/"), "/sse") {
		return "sse"
	}
	return "streamable"
}

// mcpHeadersFor 读取某 server 的认证头。
//
// 格式：Header1:Value1;Header2:Value2（分号分隔多项，冒号分隔名值）。
// 值内的冒号保留（如 "Authorization:Bearer xxx" 切第一个冒号）。
//
// 只从**进程环境**读，不从 loaded 配置读——工作区可控的值不能进认证头。
func mcpHeadersFor(serverName string) map[string]string {
	raw := strings.TrimSpace(os.Getenv(mcpHeadersPrefix + serverName))
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, item := range strings.Split(raw, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, v, ok := strings.Cut(item, ":")
		if !ok {
			continue // 无冒号 → 不是合法头，跳过而非报错（避免启动失败）
		}
		if k = strings.TrimSpace(k); k != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func runServe(args []string) int {
	fs, cfg := newFlagSet("serve", "启动渠道服务")
	// 默认 longconn：webhook 形态已移除，保留 webhook 作默认值会让
	// 不传参数时直接报错（那是个容易漏的坑）。
	mode := fs.String("feishu-mode", "longconn", "接入形态：longconn（长连接，只需出网）。webhook 形态已移除")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	loaded, err := loadConfig(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}
	printControlValues(loaded)

	// 参数校验**先于装配**：否则传了不支持的 mode 时，用户会先看到
	// 装配阶段的错误（如缺凭据），而非「模式已移除」——误导排查方向。
	if *mode != "longconn" {
		fmt.Fprintf(os.Stderr,
			"taiji serve: 未知 --feishu-mode=%q（仅支持 longconn——webhook 形态已移除）\n", *mode)
		return 2
	}

	// 凭据链自检：本包读取的凭据键必须都受 config 层保护，
	// 否则工作区文件可覆盖凭据（凭据劫持）。启动期暴露优于运行期发现。
	if err := feishu.EnsureCredentialKeysProtected(); err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}

	// ── 端到端管道装配（issue #9）──
	// 把各层串起来：门禁 → 路由 → 串行化 → 执行 → 出站。
	pipeline, err := buildPipeline(loaded, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}
	defer pipeline.Close()

	// 异步分发：立即回 200，后台处理。去重抗飞书重投。
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{
		Handler: pipeline.Pipeline,
		Deduper: channel.NewDeduper(channel.DefaultDedupTTL),
		Logf:    func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}

	// ── 长连接模式（issue #9 Wave 5，AC-6）──
	// 长连接无需验签（信任来自 SDK 与飞书的 TLS 通道，§2.2），
	// 也无需公网入口——只需出网。适用于内网部署与本地开发。
	//
	// **webhook 形态已移除**：原型只用长连接。原 webhook 分支
	// （HTTP 端点 + 验签 + URL 挑战应答）及其凭据检查随之删除。
	// 若将来需要 webhook，可从 git 历史恢复，并注意它需要公网入口。
	// 模式校验已提前到装配之前（见上）。
	return runLongConn(loaded, pipeline, dispatcher)
}

// printControlValues 打印保留键的最终生效值。
// 这是 #1 demo path 的证据面：无论工作区写了什么，这里显示的都必须来自启动环境。
func printControlValues(cfg map[string]string) {
	keys := make([]string, 0, len(config.ReservedKeys))
	for k := range config.ReservedKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintln(os.Stderr, "生效的控制值（来自受信启动环境）:")
	for _, k := range keys {
		v, ok := cfg[k]
		if !ok {
			continue
		}
		fmt.Fprintf(os.Stderr, "  %s=%s\n", k, v)
	}
}

// ── 端到端管道装配（issue #9）──

// pipelineHolder 持有管道与执行器，统一释放。
//
// 执行器（chat.Executor）持有长驻 runner，其 session service 承载多轮历史
// ——必须跨消息复用（见 chat.Executor 的文档），故它的生命周期与进程一致。
type pipelineHolder struct {
	Pipeline *server.Pipeline
	executor *chat.Executor
}

func (h *pipelineHolder) Close() {
	if h == nil || h.executor == nil {
		return
	}
	h.executor.Close()
}

// buildPipeline 装配端到端管道。
//
// 配置来源分两类（设计文档 §4.6）：
//   - 模型配置：受信启动环境（TAIJI_MODEL_*），可由工作区文件覆盖非保留键
//   - 渠道凭据：只从受信环境（FEISHU_APP_ID/APP_SECRET），工作区不得覆盖
func buildPipeline(loaded map[string]string, logw io.Writer) (*pipelineHolder, error) {
	logf := func(format string, args ...any) {
		fmt.Fprintf(logw, "[pipeline] "+format+"\n", args...)
	}

	// 模型装配：与 chat 子命令同源。
	modelCfg := bootstrap.ModelConfigFromEnv()
	if v, ok := loaded[bootstrap.EnvModelName]; ok && v != "" {
		modelCfg.Name = v
	}
	if v, ok := loaded[bootstrap.EnvModelBaseURL]; ok {
		modelCfg.BaseURL = v
	}

	// MCP 工具集（可选）：未配置则不挂工具。
	// 端到端验收线要求「回答涉及工具调用时，工具结果体现在最终回复里」。
	mcpCfgs, err := parseMCPSpecs(envMCPSpecs())
	if err != nil {
		return nil, fmt.Errorf("解析 MCP 配置: %w", err)
	}
	toolSets, err := bootstrap.NewMCPSets(mcpCfgs)
	if err != nil {
		return nil, fmt.Errorf("装配 MCP: %w", err)
	}

	// 出站：需要应用凭据换 tenant_access_token。
	// （构造移到权限校验之后——校验是纯配置检查，不需要凭据，
	// 应优先暴露配置错误，且让校验可在无凭据环境下测试。）

	// 执行器：长驻，跨消息共享 session（多轮历史的前提）。
	//
	// AllowTools 必须传：工具策略是默认拒绝 + 白名单放行（issue #4）。
	// 漏传时白名单为空 → 所有工具被拒，而模型仍看得见工具名，
	// 表现为「配了 MCP 却不生效」且无报错（issue #9 AC-2 的根因）。
	allowTools := envAllowTools()
	if len(toolSets) > 0 && len(allowTools) == 0 {
		logf("警告：已装配 %d 个 MCP server，但 TAIJI_ALLOW_TOOLS 为空——"+
			"工具策略默认拒绝，所有工具调用都会被拒。请显式列出要放行的工具名"+
			"（如 mockmcp_echo）。", len(toolSets))
	}
	// 远程 server 无认证头时提示：多数托管 MCP 服务要求 token，
	// 缺失会以 401 形式在**调用时**才暴露，启动期提示更易定位。
	for _, c := range mcpCfgs {
		if c.Transport != "stdio" && len(c.Headers) == 0 {
			logf("提示：MCP server %q 是远程（%s）但未配置认证头。"+
				"若该服务要求 token，请设 %s%s=Authorization:Bearer <token>。",
				c.Name, c.Transport, mcpHeadersPrefix, c.Name)
		}
	}

	// 用户级权限（方案 A：静态配置）。未配置时为 nil。
	//
	// **serve 路径必须有明确的用户级权限决策**（issue #6 缺口 3）：
	// 与 CLI 不同，serve 面向多个 IM 用户，缺权限表 = 任何能触发 bot 的人
	// 都能用所有已放行工具（安全边界消失）。故此处 fail-fast——
	// 有工具但无决策时拒绝启动，而非运行期静默放行。
	permissions := resolvePermissions()
	if permissions != nil {
		switch sp := permissions.(type) {
		case *authz.RBACPermissions:
			logf("用户级权限已启用：RBAC（%d 个角色 / %d 个用户）",
				sp.RoleCount(), sp.UserCount())
		case *authz.StaticPermissions:
			logf("用户级权限已启用：静态表（%d 个主体）", sp.PrincipalCount())
		default:
			logf("用户级权限已启用")
		}
		// 打印主体 ID 前缀——否则用户不知道 TAIJI_USER_PERMISSIONS 的
		// key 该写什么（workspace 段有兜底值 default，不显眼且易漏）。
		// 格式与 pipeline 注入时用的完全一致（同一 workspaceID()）。
		logf("主体 ID 前缀：%s:feishu: —— 配置的 key "+
			"应写成 <该前缀><用户open_id>，如 %s:feishu:ou_xxx",
			workspaceID(loaded), workspaceID(loaded))
	} else if allowAllUsersFromEnv() {
		logf("用户级权限：已按 %s=1 显式放开——任何能触发 bot 的用户"+
			"都可使用已放行的工具。", envAllowAllUsers)
	}
	if err := validateServePermissions(servePermInput{
		// 判据是「有工具**实际可调用**」，而非「挂了 MCP server」——
		// 白名单为空时工具策略默认拒绝一切，无边界可失，
		// 此时要求权限表是误导（用户配了表重启后才发现白名单才是问题）。
		HasTools:       len(allowTools) > 0,
		HasPermissions: permissions != nil,
		AllowAllUsers:  allowAllUsersFromEnv(),
	}); err != nil {
		bootstrap.CloseMCPSets(toolSets)
		return nil, err
	}

	// 出站：需要应用凭据换 tenant_access_token。
	// 放在权限校验之后——校验是纯配置检查，先暴露配置错误更省事。
	sender, err := feishu.NewSender(feishu.SenderConfigFromEnv(loaded))
	if err != nil {
		bootstrap.CloseMCPSets(toolSets)
		return nil, err
	}

	executor, err := chat.NewExecutor(chat.Options{
		Config:      modelCfg,
		AppName:     "taiji",
		UserID:      "feishu",
		ToolSets:    toolSets,
		AllowTools:  allowTools,
		Permissions: permissions,
		// 工具调用轮次上限：给确定性拒绝加协议层兜底（缺口 4）。
		MaxToolIterations: envMaxToolIterations(),
		Echo:              logw,
	})
	if err != nil {
		bootstrap.CloseMCPSets(toolSets)
		// 白名单校验失败是最常见的启动失败（工具名带 {server}_ 前缀，
		// 少写或写错大小写都会命中）。此时把「实际可用的工具名」打出来，
		// 用户不必从 error 文本里反推。
		if names := registeredToolNamesHint(mcpCfgs); len(names) > 0 {
			logf("提示：本次装配的 MCP server 为 %v。"+
				"工具名形如 {server名}_{远端工具名}，大小写敏感——"+
				"请以错误信息里 registered: 后面的名字为准。", serverNames(mcpCfgs))
		}
		return nil, fmt.Errorf("装配执行器: %w", err)
	}

	// 装配成功后才打印生效状态——放在 NewExecutor **之后**，
	// 否则白名单校验失败时会先打印「放行 [...]」再报错，误导为成功。
	logf("MCP server 已装配：%v", serverNames(mcpCfgs))
	if len(toolSets) > 0 {
		registered := executor.RegisteredTools()
		if len(registered) > 0 {
			logf("模型可见的工具名：%v", registered)
		}
		if allowed := executor.AllowedTools(); len(allowed) > 0 {
			logf("工具策略：默认拒绝，放行 %v", allowed)
		} else {
			logf("工具策略：默认拒绝，白名单为空——%d 个已注册工具均不可执行",
				len(registered))
		}
	}

	p, err := server.New(server.Config{
		Sender:   sender,
		Executor: executor,
		Gate:     gateConfigFromEnv(),
		Route: channel.RouteConfig{
			WorkspaceID: workspaceID(loaded),
			// 群聊按话题分流（§4.4.4 的 thread_map）；单聊无话题概念。
			BindingMode: channel.BindingThreadMap,
		},
		Logf: logf,
	})
	if err != nil {
		executor.Close()
		bootstrap.CloseMCPSets(toolSets)
		return nil, err
	}
	return &pipelineHolder{Pipeline: p, executor: executor}, nil
}

// gateConfigFromEnv 从受信环境读门禁配置。
//
// 默认值取向是 fail-closed：未配置 activation 时按 when_mentioned
// （群聊必须 @ 才响应），而不是 always——后者会让 bot 在群里对每条消息
// 都插话。私聊不受 @ 约束（门禁第 2 步放行）。
func gateConfigFromEnv() server.GateConfig {
	activation := channel.ActivationWhenMentioned
	switch os.Getenv("TAIJI_FEISHU_ACTIVATION") {
	case "always":
		activation = channel.ActivationAlways
	case "disabled":
		activation = channel.ActivationDisabled
	}
	audience := channel.AudienceEveryone
	if os.Getenv("TAIJI_FEISHU_AUDIENCE") == "owner_only" {
		audience = channel.AudienceOwnerOnly
	}
	var owners []string
	if v := strings.TrimSpace(os.Getenv("TAIJI_FEISHU_OWNERS")); v != "" {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				owners = append(owners, s)
			}
		}
	}
	return server.GateConfig{
		Activation: activation,
		Audience:   audience,
		// botOpenID 是 @ 判定的基准。未配置时群聊会被 fail-closed 拒绝
		// （门禁第 4 步）——这是刻意的：宁可拒绝也不能静默放行。
		BotOpenID: strings.TrimSpace(os.Getenv("TAIJI_FEISHU_BOT_OPEN_ID")),
		Owners:    owners,
	}
}

// workspaceID 返回串行化域与路由用的 workspace 标识。
func workspaceID(loaded map[string]string) string {
	if v := strings.TrimSpace(os.Getenv("TAIJI_WORKSPACE_ID")); v != "" {
		return v
	}
	if v := strings.TrimSpace(loaded["WORKSPACE_NAME"]); v != "" {
		return v
	}
	return "default"
}

// envMCPSpecs 从环境读 MCP server 配置（分号分隔的 name=target 列表）。
//
// target 可以是本地命令（stdio）或 http(s) URL（远程）。
// 远程 server 的认证头走 TAIJI_MCP_HEADERS_<name>（见 mcpHeadersPrefix）。
func envMCPSpecs() []string {
	v := strings.TrimSpace(os.Getenv("TAIJI_MCP_SERVERS"))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envAllowTools 从环境读工具白名单（逗号分隔的模型可见工具名）。
//
// 为什么 serve 也需要它：工具策略是「默认拒绝 + 白名单放行」（issue #4），
// 空白名单意味着**全部拒绝**。若 serve 路径没有白名单通道，配了 MCP server
// 也永远无法执行——模型看得见工具、调用被策略拒，且没有任何报错
// （issue #9 AC-2 在真实平台无法验证的根因）。
//
// 分隔符用逗号，与 TAIJI_FEISHU_OWNERS 一致。
func envAllowTools() []string {
	v := strings.TrimSpace(os.Getenv("TAIJI_ALLOW_TOOLS"))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runLongConn 以长连接模式运行（issue #9 Wave 5，AC-6）。
//
// 与 webhook 模式的关键差异：
//   - 无 HTTP 端点、无验签（§2.2：长连接不需要 verificationToken/encryptKey）
//   - 入站由 SDK 回调直出，投递给同一个 dispatcher（去重 + 异步处理）
//   - 关闭必须调 LongConn.Stop() → SDK 的 Close()。**只取消 ctx 不够**：
//     larkws.Client.Start 末尾是裸 select{}（ws/client.go:206-232），
//     不观察 ctx，socket 会存活并自动重连（AC-6 要防的正是这个）。
func runLongConn(loaded map[string]string, pipeline *pipelineHolder, dispatcher *server.Dispatcher) int {
	senderCfg := feishu.SenderConfigFromEnv(loaded)
	if senderCfg.AppID == "" || senderCfg.AppSecret == "" {
		fmt.Fprintf(os.Stderr,
			"taiji serve: 长连接模式需要 %s 与 %s（凭据只从启动环境读，见设计文档 §4.6）。\n",
			feishu.EnvAppID, feishu.EnvAppSecret)
		return 1
	}

	lc, err := feishu.NewLongConn(feishu.LongConnConfig{
		AppID:     senderCfg.AppID,
		AppSecret: senderCfg.AppSecret,
		// 入站复用同一套去重 + 异步分发——两条入站路径的
		// 下游行为必须一致，否则语义会因入口不同而分叉。
		OnMessage: dispatcher.Enqueue,
		Logf:      func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := lc.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: 启动长连接失败: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "taiji serve: 长连接已启动（无需公网入口）")

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\ntaiji serve: 收到中断信号，正在关闭…")

	// 顺序：先停长连接（不再收新消息）→ 再停分发器（等在途处理完）。
	// Stop 内部调 SDK 的 Close()，真正断开 socket（AC-6）。
	lc.Stop()
	if err := lc.StartErr(); err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: 长连接异常退出: %v\n", err)
	}
	dispatcher.Stop()
	fmt.Fprintln(os.Stderr, "taiji serve: 已关闭")
	return 0
}

// serverNames 提取 MCP server 名（用于日志）。
func serverNames(cfgs []bootstrap.MCPServerConfig) []string {
	out := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, c.Name)
	}
	return out
}

// registeredToolNamesHint 在装配失败时给出可诊断的线索。
//
// 白名单校验失败（工具名未注册）是最常见的启动失败——工具名是
// {server名}_{远端工具名} 拼出来的，且**大小写敏感**。
// 这里只需确认「本次确实配了 server」，具体名字由 authz 的错误信息列出
// （validateAllowList 会打印 registered: ... 列表）。
func registeredToolNamesHint(cfgs []bootstrap.MCPServerConfig) []string {
	if len(cfgs) == 0 {
		return nil
	}
	return serverNames(cfgs)
}

// envPermissions 从环境读用户级权限表（方案 A：静态配置）。
//
// 格式：条目用分号分隔，主体与工具列表用等号分隔，工具用逗号分隔。
//
//	TAIJI_USER_PERMISSIONS="ws1:feishu:ou_alice=mockmcp_echo,infraverse_*;ws1:feishu:ou_admin=*"
//
// 主体 ID 形态为 {workspace}:{platform}:{open_id}（见 authz.ResolvePrincipal），
// 与管道注入的形态一致。工具名支持 "*" 与 "prefix_*" 通配。
//
// 返回 nil 表示未配置——此时不做用户级判定（向后兼容）。
// 配置了但解析出空表 → 返回空表（拒绝一切），与"未配置"语义不同。
func envPermissions() authz.PermissionSource {
	raw := strings.TrimSpace(os.Getenv("TAIJI_USER_PERMISSIONS"))
	if raw == "" {
		return nil
	}
	table := make(map[string][]string)
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		principal, tools, ok := strings.Cut(entry, "=")
		if !ok {
			continue // 无 = 的条目跳过（不因一处笔误导致启动失败）
		}
		principal = strings.TrimSpace(principal)
		if principal == "" {
			continue
		}
		var list []string
		for _, t := range strings.Split(tools, ",") {
			if t = strings.TrimSpace(t); t != "" {
				list = append(list, t)
			}
		}
		if len(list) > 0 {
			table[principal] = list
		}
	}
	return authz.NewStaticPermissions(table)
}

// defaultMaxToolIterations 是工具调用轮次上限的默认值。
//
// 取值理由：正常的多步工具任务典型 2-4 轮，8 足够宽裕；而病态重试
// （模型反复调用被拒工具）会在 8 轮内被终止。这个默认值是「缺口 4」
// 真正修好的前提——不设默认等于缺口在新部署上依然敞开。
//
// 代价（明示）：极长的合法工具链（>8 轮）会被截断。此时 finalization
// 会做一次无工具调用，把已有信息汇总成回答——用户得到的是「部分结果」
// 而非错误。确需更长链路的部署可用 TAIJI_MAX_TOOL_ITERATIONS 调大。
const defaultMaxToolIterations = 8

// envMaxToolIterations 从环境读工具调用轮次上限。
//
//	TAIJI_MAX_TOOL_ITERATIONS=20   # 调大
//	TAIJI_MAX_TOOL_ITERATIONS=0    # 显式关闭（不限制，退回框架默认行为）
//
// 未设时返回 defaultMaxToolIterations。非法值（非数字）也返回默认值——
// 宁可保守也不因一处笔误让上限消失（fail-closed 取向）。
func envMaxToolIterations() int {
	v := strings.TrimSpace(os.Getenv("TAIJI_MAX_TOOL_ITERATIONS"))
	if v == "" {
		return defaultMaxToolIterations
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultMaxToolIterations
	}
	return n
}

// envRBAC 从环境读 RBAC 配置（Wave 2——决策三）。
//
// 格式（分号分隔条目，等号分隔名与值）：
//
//	TAIJI_RBAC="role:admin=*;role:operator=mockmcp_echo,infraverse_*;user:ws1:feishu:ou_alice=admin;user:ws1:feishu:ou_bob=operator"
//
// 三类条目（前缀区分）：
//   - role:<角色名>=<权限点列表>      定义角色 → 权限
//   - parent:<子角色>=<父角色列表>     定义 RBAC1 继承
//   - user:<主体ID>=<角色列表>        绑定用户 → 角色
//
// 主体 ID 形态为 {workspace}:{platform}:{open_id}（与 authz.ResolvePrincipal 一致）。
// 权限点支持 "*" 与 "prefix_*" 通配（复用 matchToolPattern 语义）。
//
// 返回 nil 表示未配置——此时回退到 TAIJI_USER_PERMISSIONS（迁移期并存）。
func envRBAC() authz.PermissionSource {
	raw := strings.TrimSpace(os.Getenv("TAIJI_RBAC"))
	if raw == "" {
		return nil
	}
	roles := make(map[string][]string)
	userRoles := make(map[string][]string)
	roleParents := make(map[string][]string)

	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue // 无 = 的条目跳过（不因一处笔误导致启动失败）
		}
		key = strings.TrimSpace(key)
		items := splitCSV(value)
		if len(items) == 0 {
			continue
		}

		kind, name, ok := strings.Cut(key, ":")
		if !ok {
			continue // 无前缀的条目跳过
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		switch strings.TrimSpace(kind) {
		case "role":
			roles[name] = items
		case "parent":
			roleParents[name] = items
		case "user":
			userRoles[name] = items
		}
	}

	return authz.NewRBACPermissions(authz.RBACConfig{
		Roles:       roles,
		UserRoles:   userRoles,
		RoleParents: roleParents,
	})
}

// splitCSV 按逗号切分并去空白，丢弃空项。
func splitCSV(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// resolvePermissions 返回生效的用户级权限源。
//
// 优先级：TAIJI_RBAC（RBAC）> TAIJI_USER_PERMISSIONS（静态表）。
// 两者都未配 → nil（不做用户级判定；serve 路径会因此拒绝启动，见
// validateServePermissions）。
//
// 为什么并存而非替换：这是迁移期——现有部署可能已在用
// TAIJI_USER_PERMISSIONS，直接替换会让它们静默失效（项目取向：
// 静默失效最难排查）。RBAC 是更完整的模型，新部署应优先用它。
func resolvePermissions() authz.PermissionSource {
	if p := envRBAC(); p != nil {
		return p
	}
	return envPermissions()
}
