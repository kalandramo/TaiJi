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
	"time"

	"github.com/kalandramo/TaiJi/internal/agentreg"
	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/bootstrap"
	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/channel/cmd"
	"github.com/kalandramo/TaiJi/internal/channel/feishu"
	"github.com/kalandramo/TaiJi/internal/chat"
	"github.com/kalandramo/TaiJi/internal/config"
	"github.com/kalandramo/TaiJi/internal/server"

	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/tool"
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
	// 若用户设了权限配置（误以为对 CLI 生效），显式提示而非静默忽略——
	// 否则「配了却不生效」又是一个静默缺口。
	// 这里只判断「是否配了」，不校验（CLI 不消费权限，配置错误留待 serve 暴露）。
	//
	// 只认 TAIJI_RBAC：TAIJI_USER_PERMISSIONS 已于 2026-09-26 退役，
	// 探测它没有意义（设了也无效，且没有迁移价值——它本就不对 CLI 生效）。
	if strings.TrimSpace(os.Getenv("TAIJI_RBAC")) != "" {
		fmt.Fprintf(os.Stderr,
			"taiji chat: 注意——用户级权限配置（TAIJI_RBAC）"+
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
//
// 输出经 redactControlValue 脱敏——控制值里含凭据（TAIJI_AGENTS 嵌
// app_secret，TAIJI_MODEL_API_KEY 等键名即敏感），而这段会进 CI 归档、
// 也会被用户贴出来排障。
func printControlValues(cfg map[string]string) {
	printControlValuesTo(os.Stderr, cfg)
}

// printControlValuesTo 是可测版本（注入 writer，验证真实打印链的脱敏）。
func printControlValuesTo(w io.Writer, cfg map[string]string) {
	keys := make([]string, 0, len(config.ReservedKeys))
	for k := range config.ReservedKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintln(w, "生效的控制值（来自受信启动环境）:")
	for _, k := range keys {
		v, ok := cfg[k]
		if !ok {
			continue
		}
		fmt.Fprintf(w, "  %s=%s\n", k, redactControlValue(k, v))
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

	// agent 配置（多 agent + 每 agent 独立飞书应用）。
	// 未配 TAIJI_AGENTS 时回退单 agent（读 FEISHU_APP_ID/SECRET）。
	agentSpecs, err := parseAgents()
	if err != nil {
		return nil, err
	}
	if len(agentSpecs) > 1 {
		var names []string
		for _, a := range agentSpecs {
			names = append(names, a.Name)
		}
		logf("多 agent 模式：%v（每个 agent 独立飞书应用与 session）", names)
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

	// 用户级权限（唯一来源：TAIJI_RBAC）。未配置时为 nil。
	//
	// **serve 路径必须有明确的用户级权限决策**（issue #6 缺口 3）：
	// 与 CLI 不同，serve 面向多个 IM 用户，缺权限表 = 任何能触发 bot 的人
	// 都能用所有已放行工具（安全边界消失）。故此处 fail-fast——
	// 有工具但无决策时拒绝启动，而非运行期静默放行。
	//
	// RBAC 配置有误（引用未定义角色）时 resolvePermissions 返回 error，
	// 同样 fail-fast——否则该用户会被静默拒绝（「配了却不生效」）。
	permissions, err := resolvePermissions()
	if err != nil {
		bootstrap.CloseMCPSets(toolSets)
		return nil, err
	}
	if permissions != nil {
		switch sp := permissions.(type) {
		case *authz.RBACPermissions:
			logf("用户级权限已启用：RBAC（%d 个角色 / %d 个用户）",
				sp.RoleCount(), sp.UserCount())
		case *authz.StaticPermissions:
			// 生产路径不应到达：resolvePermissions 只返回 RBAC。
			// 保留分支是为库内其他 PermissionSource 实现留出日志位置。
			logf("用户级权限已启用：静态表（%d 个主体）", sp.PrincipalCount())
		default:
			logf("用户级权限已启用")
		}
		// 打印主体 ID 前缀——否则用户不知道 TAIJI_RBAC 的
		// user: 条目该写什么（workspace 段有兜底值 default，不显眼且易漏）。
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
	//
	// 多 agent 部署下的语义：真正的出站在 buildAgents 里按 agent 各建一个
	// （凭据隔离），这个全局 sender **不会被用到**（server 按 agent 查
	// Senders map）。但它不能为 nil——server.New 硬性要求非 nil。
	// 故用**第一个 agent 的凭据**构造它：语义自洽（不是凭空要求用户
	// 再配一套用不上的全局凭据），且失败时错误指向具体的 agent。
	senderCfg := feishu.SenderConfigFromEnv(loaded)
	if len(agentSpecs) > 1 {
		senderCfg = feishu.SenderConfig{
			AppID:     agentSpecs[0].AppID,
			AppSecret: agentSpecs[0].AppSecret,
		}
	}
	sender, err := feishu.NewSender(senderCfg)
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
		// 系统提示（2026-09-26 补）：serve 此前**完全没有**这条通道——
		// CLI 有 -instruction flag，serve 只能靠框架默认 instruction。
		// skill 的静态摘要也由此进入模型上下文。
		Instruction:      envInstruction(),
		SkillRoot:        envSkillRoot(),
		SkillToolProfile: envSkillToolProfile(),
		Echo:             logw,
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
	// 工具面提示**不再以 len(toolSets)>0 为门槛**（2026-09-26 改）：
	// skill 也会注册工具（skill_load 等），skill-only 部署（无 MCP）
	// 此前会完全静默——用户看不到白名单为空，直到某次调用被拒才困惑。
	registered := executor.RegisteredTools()
	if len(registered) > 0 {
		logf("模型可见的工具名：%v", registered)
	}
	if allowed := executor.AllowedTools(); len(allowed) > 0 {
		logf("工具策略：默认拒绝，放行 %v", allowed)
	} else if len(registered) > 0 {
		logf("工具策略：默认拒绝，白名单为空——%d 个已注册工具均不可执行",
			len(registered))
	}
	// skill 专项提示：skill 工具同样受白名单管辖，但用户容易以为
	// 「配了 TAIJI_SKILLS_ROOT 就能用」。此处显式点出未放行的 skill 工具。
	if root := envSkillRoot(); root != "" {
		var notAllowed []string
		allowedSet := make(map[string]bool)
		for _, a := range executor.AllowedTools() {
			allowedSet[a] = true
		}
		for _, n := range registered {
			if strings.HasPrefix(n, "skill_") && !allowedSet[n] {
				notAllowed = append(notAllowed, n)
			}
		}
		if len(notAllowed) > 0 {
			logf("提示：skill 已启用（root=%s），但以下 skill 工具未在 "+
				"TAIJI_ALLOW_TOOLS 中放行，模型无法调用：%v。"+
				"若要使用，请把它们加入 TAIJI_ALLOW_TOOLS。",
				root, notAllowed)
		} else {
			logf("skill 已启用（root=%s），读类工具已放行。", root)
		}
	}

	// 命令系统装配（SPEC P5）。
	//
	// 装配顺序的关键：CancelRegistry 必须**先创建**，再同时传给
	// cmd.Deps（/stop 的 Handler 用）与 server.Config（管道执行 run 时用）。
	// 两者必须是**同一实例**——否则 /stop 取消的是另一个注册表里的 run。
	gateCfg := gateConfigFromEnv()

	// 多 agent 部署：按各 agent 的凭据取 bot open_id，构造 AppID → open_id 映射。
	//
	// 为什么必须按 agent 取：每个 bot 的 open_id 不同，门禁要拿**接收该消息
	// 的那个 bot** 的 open_id 去比对 mentions。用全局单值会让「@ bot B」被判成
	// not_mentioned（2026-10-02 真实双 agent 同群故障）。
	//
	// 为什么自动取而非让人手填：open_id 是应用维度的，手填既易错（复制粘贴
	// 串号）又在换应用时要求同步改配置。凭据已在手，open_id 是它的函数。
	//
	// 失败即中止：取不到 open_id 的 bot 会被门禁 fail-closed 拒绝所有群消息
	// （表现为「bot 活着但永远不响应」），静默继续比启动失败更难排查。
	if len(agentSpecs) > 1 {
		botCtx, cancelBot := ctxForBotInfo()
		mapping, err := fetchBotOpenIDs(botCtx, agentSpecs)
		cancelBot()
		if err != nil {
			executor.Close()
			bootstrap.CloseMCPSets(toolSets)
			return nil, err
		}
		gateCfg.BotOpenIDs = mapping
		logf("门禁：已按 agent 取 bot open_id（%d 个）", len(mapping))
	}

	wsID := workspaceID(loaded)
	cancels := server.NewCancelRegistry()
	sessionGens := newSessionGenerations()

	cmdRegistry := cmd.NewRegistry()
	cmd.RegisterBuiltins(cmdRegistry, cmd.Deps{
		ClearSession: sessionGens.Next,
		CancelRun:    cancels.Cancel,
	})

	// ── 多 agent 装配（形态 C：Bot 即 agent）──
	//
	// 每个 agent 一套 executor + sender：前者保证 session 与工具面隔离，
	// 后者保证**凭据隔离**（回复必须来自该 agent 绑定的飞书应用）。
	//
	// 单 agent 部署（TAIJI_AGENTS 未配）时 agents==nil，
	// 上面的 executor/sender 单值即可，行为与改动前完全一致。
	agentsReg, executors, senders, err := buildAgents(
		agentSpecs, modelCfg, allowTools, permissions, toolSets, logw)
	if err != nil {
		executor.Close()
		bootstrap.CloseMCPSets(toolSets)
		return nil, err
	}

	p, err := server.New(server.Config{
		Sender:   sender,
		Executor: executor,
		Gate:     gateCfg,
		// 多 agent（nil 表示未启用，走上面的单值路径）。
		Agents:    agentsReg,
		Executors: executors,
		Senders:   senders,
		Route: channel.RouteConfig{
			WorkspaceID: wsID,
			// 群聊按话题分流（§4.4.4 的 thread_map）；单聊无话题概念。
			BindingMode: channel.BindingThreadMap,
		},
		Logf: logf,
		// ── 命令系统 ──
		Commands:    cmdRegistry,
		Permissions: permissions,
		// owner 判定复用门禁的 owner 列表（authz.IsOwner 是纯函数比对）。
		OwnerCheck: func(openID string) bool {
			return authz.IsOwner(gateCfg.Owners, openID)
		},
		SessionGen: sessionGens,
		Cancels:    cancels,
	})
	if err != nil {
		executor.Close()
		bootstrap.CloseMCPSets(toolSets)
		return nil, err
	}

	logf("命令系统已装配：%v", cmdRegistry.Names())

	// 命令权限提示。
	//
	// **为什么不能只说「未配权限源」**：用户可能配了 TAIJI_USER_PERMISSIONS
	// （工具权限），它**不含 cmd: 权限点**——命令仍会被全部拒绝，但
	// 用户看到「已启用」会以为没问题。故提示必须点明「需要 cmd: 权限点」。
	if permissions == nil {
		logf("警告：未配用户级权限源，命令将全部被拒（fail-closed）。" +
			"配 TAIJI_RBAC 并在 role 权限列表里加上 cmd:help、cmd:stop 等即可放行。" +
			"例外：/whoami 无需权限，可先用它查出自己的主体 ID（配 RBAC 要用）")
	} else {
		logf("命令权限点形如 cmd:help、cmd:status、cmd:clear、cmd:stop——" +
			"须在 TAIJI_RBAC 的 role 权限列表里**显式**列出；" +
			"TAIJI_USER_PERMISSIONS（工具权限）不含它们。" +
			"例外：/whoami 无需权限（它用于查你自己的主体 ID，" +
			"而配 TAIJI_RBAC 正需要该 ID——若也要求权限则形成死锁）")
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

// ctxForBotInfo 给启动期的 bot info 调用一个带超时的 ctx。
//
// 返回 cancel 由调用方 defer——丢弃它会泄漏 ctx（go vet 会报
// "the cancel function returned by context.WithTimeout should be called"）。
//
// 启动期一次性调用，无外部取消源；超时防止网络问题让启动永久挂住
// （用户会以为服务卡死，而实际只是在等一个 HTTP 响应）。
func ctxForBotInfo() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), botInfoTimeout)
}

// botInfoTimeout 是单个 bot info 请求的时限。
const botInfoTimeout = 15 * time.Second

// fetchBotOpenIDs 按各 agent 的凭据取 bot open_id，返回 AppID → open_id 映射。
//
// 用于门禁的 per-agent @ 判定（每个 bot 的 open_id 不同）。
//
// 失败即返回 error（fail-fast）：某个 agent 取不到 open_id，它的群消息
// 会被门禁 fail-closed 全拒——表面是「bot 在线但不理人」，用户无从判断
// 是权限、门禁还是网络问题。启动即报错比这好得多，且错误里点名是哪个 agent。
//
// 串行调用而非并发：agent 数是个位数、各一次 HTTP、启动期只跑一次，
// 并发的复杂度换不来可感知的收益。
func fetchBotOpenIDs(ctx context.Context, specs []agentSpec) (map[string]string, error) {
	out := make(map[string]string, len(specs))
	for _, spec := range specs {
		openID, err := feishu.FetchBotOpenID(ctx, spec.AppID, spec.AppSecret)
		if err != nil {
			return nil, fmt.Errorf(
				"获取 agent %q 的 bot open_id 失败（app_id=%s）：%w\n"+
					"提示：该 open_id 用于群聊 @ 判定，取不到则此 bot 的群消息会被拒绝",
				spec.Name, spec.AppID, err)
		}
		out[spec.AppID] = openID
	}
	return out, nil
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
	// 多 agent 部署：每个 agent 一条长连接（各自的应用凭据）。
	// 单 agent 时退化为一条，与改动前一致。
	specs, err := parseAgents()
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}

	logfErr := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }

	// 逐 agent 建长连接。任一失败即整体失败——缺一条连接意味着
	// 某个 bot 的消息永远收不到，静默降级会造成「部分功能可用」的假象。
	var conns []*feishu.LongConn
	for _, spec := range specs {
		if spec.AppID == "" || spec.AppSecret == "" {
			fmt.Fprintf(os.Stderr,
				"taiji serve: agent %q 缺少飞书凭据（%s / %s）。\n"+
					"凭据只从启动环境读（见设计文档 §4.6）。\n",
				spec.Name, feishu.EnvAppID, feishu.EnvAppSecret)
			for _, c := range conns {
				c.Stop()
			}
			return 1
		}
		lc, cerr := feishu.NewLongConn(feishu.LongConnConfig{
			AppID:     spec.AppID,
			AppSecret: spec.AppSecret,
			// 入站复用同一套去重 + 异步分发——两条入站路径的
			// 下游行为必须一致，否则语义会因入口不同而分叉。
			OnMessage: dispatcher.Enqueue,
			Logf:      logfErr,
		})
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "taiji serve: agent %q 建长连接: %v\n", spec.Name, cerr)
			for _, c := range conns {
				c.Stop()
			}
			return 1
		}
		conns = append(conns, lc)
	}
	if len(conns) > 1 {
		fmt.Fprintln(os.Stderr, "taiji serve: 多 agent 长连接已装配", len(conns), "条")
	}

	ctx, cancel := signalContext()
	defer cancel()

	// 逐条启动。任一失败则回滚已启动的——避免留下半启动状态
	// （部分 bot 收消息、部分收不到，排查成本高）。
	for i, c := range conns {
		if err := c.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "taiji serve: 启动长连接 #%d 失败: %v\n", i+1, err)
			for _, started := range conns[:i] {
				started.Stop()
			}
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "taiji serve: 长连接已启动（%d 条，无需公网入口）\n", len(conns))

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\ntaiji serve: 收到中断信号，正在关闭…")

	// 顺序：先停长连接（不再收新消息）→ 再停分发器（等在途处理完）。
	// Stop 内部调 SDK 的 Close()，真正断开 socket（AC-6）。
	// 多条连接逐个停——SDK 的 Start 阻塞于裸 select{} 不观察 ctx，
	// 只取消 ctx 不会断连（见 runLongConn 的注释）。
	for i, c := range conns {
		c.Stop()
		if err := c.StartErr(); err != nil {
			fmt.Fprintf(os.Stderr, "taiji serve: 长连接 #%d 异常退出: %v\n", i+1, err)
		}
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

// 注：TAIJI_USER_PERMISSIONS 已于 2026-09-26 退役（由 TAIJI_RBAC 取代）。
// 其**值**早已不被读取；曾有的迁移告警已于 2026-10-02 移除——
// 旧变量残留的每种场景都已有独立信号，无需重复提示：
//   - 有工具但没配 RBAC → serve 拒绝启动，错误已指向 TAIJI_RBAC
//   - 配了 TAIJI_ALLOW_ALL_USERS=1 → 启动日志已有「显式放开」提示
//   - 无工具 → 权限本就不参与判定

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

// buildAgents 为多 agent 部署构造注册表 + 每 agent 的 executor 与 sender。
//
// 返回 (nil, nil, nil, nil) 表示**未启用多 agent**——调用方走单值路径，
// 行为与改动前完全一致（向后兼容的关键）。
//
// 只在 spec 数 > 1 时才构造多 agent：单 agent 时复用 buildPipeline 里
// 已建好的 executor/sender，避免重复装配。
func buildAgents(
	specs []agentSpec,
	modelCfg bootstrap.ModelConfig,
	allowTools []string,
	permissions authz.PermissionSource,
	toolSets []tool.ToolSet,
	logw io.Writer,
) (*agentreg.Registry, map[string]server.Executor, map[string]channel.Sender, error) {
	if len(specs) <= 1 {
		return nil, nil, nil, nil
	}

	// 共享 session 存储：agent 间历史隔离靠 session 键的 agent 前缀
	// （见 chat.scopedSessionID），而非各自的存储——后者会让内存
	// 随 agent 数线性增长。
	sharedSessions := inmemory.NewSessionService()

	entries := make([]agentreg.Entry, 0, len(specs))
	executors := make(map[string]server.Executor, len(specs))
	senders := make(map[string]channel.Sender, len(specs))

	// 已建好的 agent 按名索引——父装配时需要拿到子的实例。
	builtAgents := make(map[string]agent.Agent, len(specs))

	byName := make(map[string]agentSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}

	// **拓扑序装配**：先建叶子（无未建之子），再逐层向上。
	//
	// 为什么不能单趟循环：父需要把**已构造好的子实例**传给
	// WithSubAgents，而子可能声明在父之后（TAIJI_AGENTS 的条目顺序
	// 不应成为约束）。
	//
	// parseAgents 的 validateParents 已保证无环，故必然终止；
	// 下面的 !progressed 分支是防御（防有人绕过 parseAgents 传 specs）。
	remaining := append([]agentSpec(nil), specs...)
	for len(remaining) > 0 {
		progressed := false
		var next []agentSpec
		for _, spec := range remaining {
			children, ready := collectChildren(spec, byName, builtAgents)
			if !ready {
				next = append(next, spec)
				continue
			}
			if err := buildOne(spec, children, modelCfg, allowTools, permissions,
				toolSets, sharedSessions, logw, executors, senders, builtAgents,
				&entries); err != nil {
				return nil, nil, nil, err
			}
			progressed = true
		}
		if !progressed {
			return nil, nil, nil, fmt.Errorf(
				"装配 agent：拓扑序无法推进（剩余 %d 个）——父子关系可能有环",
				len(remaining))
		}
		remaining = next
	}

	reg, err := agentreg.New(entries)
	if err != nil {
		return nil, nil, nil, err
	}
	return reg, executors, senders, nil
}

// collectChildren 收集该 spec 的子 agent 实例。
//
// 第二个返回值表示「所有子都已建好」——用于拓扑序推进判定。
// 叶子 agent（无子）恒返回 (nil, true)。
func collectChildren(
	spec agentSpec,
	byName map[string]agentSpec,
	built map[string]agent.Agent,
) ([]agent.Agent, bool) {
	var children []agent.Agent
	for _, other := range byName {
		if other.Parent != spec.Name {
			continue
		}
		child, ok := built[other.Name]
		if !ok {
			return nil, false // 有子尚未建好，本轮跳过
		}
		children = append(children, child)
	}
	return children, true
}

// buildOne 装配单个 agent（executor + sender），并登记到各索引。
//
// 隔离参数取值优先级：**spec 字段 > 全局环境变量**。
// 后者保证未配 spec 字段的 agent 行为与改动前一致（向后兼容）。
func buildOne(
	spec agentSpec,
	children []agent.Agent,
	modelCfg bootstrap.ModelConfig,
	globalAllowTools []string,
	permissions authz.PermissionSource,
	toolSets []tool.ToolSet,
	sessions session.Service,
	logw io.Writer,
	executors map[string]server.Executor,
	senders map[string]channel.Sender,
	built map[string]agent.Agent,
	entries *[]agentreg.Entry,
) error {
	// N2：提示词。spec 未配则用全局。
	instruction := spec.Instruction
	if instruction == "" {
		instruction = envInstruction()
	}
	// N3：skill 仓库根。spec 未配则用全局。
	skillRoot := spec.Skills
	if skillRoot == "" {
		skillRoot = envSkillRoot()
	}
	// N4：MCP 工具白名单。spec 未配则用全局。
	//
	// 共享 toolSet 实例，用白名单隔离可见工具——不按 agent 各建一套
	// MCP 连接（那会让连接数乘 agent 数）。
	allowTools := spec.AllowTools
	if len(allowTools) == 0 {
		allowTools = globalAllowTools
	}

	ex, err := chat.NewExecutor(chat.Options{
		Config:            modelCfg,
		AppName:           "taiji",
		UserID:            "feishu",
		AgentName:         spec.Name,
		SubAgents:         children, // N1：父子
		SessionService:    sessions,
		ToolSets:          toolSets,
		AllowTools:        allowTools,
		Permissions:       permissions,
		MaxToolIterations: envMaxToolIterations(),
		Instruction:       instruction,
		SkillRoot:         skillRoot,
		SkillToolProfile:  envSkillToolProfile(),
		Echo:              logw,
	})
	if err != nil {
		return fmt.Errorf("装配 agent %q 的执行器: %w", spec.Name, err)
	}

	// 每个 agent 一个 sender（**凭据隔离的落点**）。
	// Sender 无 Close——SDK client 持有连接池但无需显式释放。
	sd, err := feishu.NewSender(feishu.SenderConfig{
		AppID:     spec.AppID,
		AppSecret: spec.AppSecret,
	})
	if err != nil {
		ex.Close()
		return fmt.Errorf("装配 agent %q 的出站端: %w", spec.Name, err)
	}

	executors[spec.Name] = ex
	senders[spec.Name] = sd
	built[spec.Name] = ex.Agent()
	*entries = append(*entries, agentreg.Entry{AppID: spec.AppID, Agent: spec.Name})
	return nil
}

// envInstruction 读 serve 路径的系统提示。未配返回空。
//
// 为什么需要（2026-09-26 发现的缺口）：CLI 有 -instruction flag
// （main.go:104），但 serve 装配此前**没有该字段**——飞书场景下模型
// 拿到的是框架默认 instruction，用户无法施加任何引导。
//
// 为什么不用 CLI 的 flag：serve 是常驻进程，没有命令行交互，
// 环境变量是与既有配置面（TAIJI_RBAC 等）一致的选择。
func envInstruction() string {
	return strings.TrimSpace(os.Getenv("TAIJI_INSTRUCTION"))
}

// envSkillRoot 读 skill 仓库根目录。未配返回空（不启用 skill）。
//
// 格式：目录路径，可含多个（os.PathListSeparator 分隔）。
// 每个子目录含一个 SKILL.md（上游 skill.FSRepository 约定）。
func envSkillRoot() string {
	return strings.TrimSpace(os.Getenv("TAIJI_SKILLS_ROOT"))
}

// envSkillToolProfile 读 skill 工具档位。未配返回空（用上游默认）。
//
// 上游取值："full" | "knowledge-only"。空串时 newAgent 不传该 Option，
// 上游按默认（KnowledgeOnly）处理——只注册读类 skill 工具，不含执行类。
// 这是有意选择：serve 面向 IM 用户，执行类工具（skill_run 等）
// 与「IM 来源只读」的语义冲突。
func envSkillToolProfile() string {
	return strings.TrimSpace(os.Getenv("TAIJI_SKILL_TOOL_PROFILE"))
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
// 返回 (nil, nil) 表示未配置——此时回退到 TAIJI_USER_PERMISSIONS（迁移期并存）。
//
// 配置有误（如引用未定义角色）时返回 error——**fail-fast**，而非静默
// 让部分用户被拒。这是「配了却不生效」类静默失效的根治（与缺口 3 同源）。
func envRBAC() (authz.PermissionSource, error) {
	raw := strings.TrimSpace(os.Getenv("TAIJI_RBAC"))
	if raw == "" {
		return nil, nil
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

	cfg := authz.RBACConfig{
		Roles:       roles,
		UserRoles:   userRoles,
		RoleParents: roleParents,
	}
	// 校验：所有被引用的角色必须已定义——否则用户会被静默拒绝。
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("TAIJI_RBAC 配置错误：%w", err)
	}
	return authz.NewRBACPermissions(cfg), nil
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
// 唯一来源是 TAIJI_RBAC。两者都未配 → (nil, nil)（不做用户级判定；
// serve 路径会因此拒绝启动，见 validateServePermissions）。
//
// RBAC 配置有误时返回 error——调用方应 fail-fast。
//
// **TAIJI_USER_PERMISSIONS 已退役**（2026-09-26）：其静态表模型是
// RBAC 的退化情形（User 直连 Permission，无角色、无继承），且两者
// 并存引入了一条"优先级排他"规则——用户在 A 里配的权限被 B 静默
// 覆盖，正是项目最忌讳的静默失效。保留 RBAC 一个入口后，
// "配了却不生效"的整类问题消失。
//
// 退役后仍检测老变量并告警（warnRetiredUserPermissions），
// 把静默失效转为有声失效。
func resolvePermissions() (authz.PermissionSource, error) {
	return envRBAC()
}
