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
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

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
	fs.Var(&mcpSpecs, "mcp", "挂载 MCP server，格式 name=command [args...]（可重复）")
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
	mcpCfgs, err := parseMCPSpecs(mcpSpecs)
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

	err = chat.Run(ctx, os.Stdin, chat.Options{
		Config:      modelCfg,
		Instruction: *instruction,
		ToolSets:    toolSets,
		AllowTools:  allowTools,
		Out:         os.Stdout,
		Echo:        os.Stderr,
		Debug:       *debug,
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

// parseMCPSpecs 把 "name=command [args...]" 形式的 flag 解析成配置。
func parseMCPSpecs(specs []string) ([]bootstrap.MCPServerConfig, error) {
	out := make([]bootstrap.MCPServerConfig, 0, len(specs))
	for _, s := range specs {
		name, rest, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --mcp %q: expected name=command [args...]", s)
		}
		fields := strings.Fields(strings.TrimSpace(rest))
		if len(fields) == 0 {
			return nil, fmt.Errorf("invalid --mcp %q: command is empty", s)
		}
		out = append(out, bootstrap.MCPServerConfig{
			Name:      strings.TrimSpace(name),
			Transport: "stdio",
			Command:   fields[0],
			Args:      fields[1:],
		})
	}
	return out, nil
}

func runServe(args []string) int {
	fs, cfg := newFlagSet("serve", "启动渠道服务")
	addr := fs.String("addr", ":8080", "监听地址")
	mode := fs.String("feishu-mode", "webhook", "接入形态：webhook（默认，需公网 URL）| longconn（长连接，只需出网）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	loaded, err := loadConfig(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}
	printControlValues(loaded)

	// 凭据链自检：本包读取的凭据键必须都受 config 层保护，
	// 否则工作区文件可覆盖凭据（凭据劫持）。启动期暴露优于运行期发现。
	if err := feishu.EnsureCredentialKeysProtected(); err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}

	// 凭据来自受信配置（已过滤保留键），不落配置文件。
	//
	// 注意：webhook 专属的验签凭据检查**不在这里**——它在 webhook 分支内。
	// 长连接模式不验签（信任来自 SDK 与飞书的 TLS 通道，§2.2），
	// 在此处检查会打印一条误导性警告（「webhook 端点将拒绝所有回调」），
	// 而长连接模式下根本没有 webhook 端点。

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
	if *mode == "longconn" {
		return runLongConn(loaded, pipeline, dispatcher)
	}
	if *mode != "webhook" {
		fmt.Fprintf(os.Stderr, "taiji serve: 未知 --feishu-mode=%q（支持 webhook | longconn）\n", *mode)
		return 2
	}

	// webhook 专属：验签凭据检查（长连接不需要，见上方注释）。
	verifyCfg := feishu.VerifyConfigFromEnv(loaded)
	if verifyCfg.VerificationToken == "" {
		// fail-closed：不配置就不启动，而不是启动一个拒绝一切请求的端点
		// 让运维以为服务已就绪。这里把「配置缺失」和「端点拒绝」分开表达。
		fmt.Fprintf(os.Stderr,
			"taiji serve: %s 未配置——webhook 端点将拒绝所有回调（fail-closed）。\n"+
				"  设置方式：在启动环境中导出该变量（凭据不写配置文件，见设计文档 §4.6）。\n",
			feishu.EnvVerificationToken)
	}

	h := feishu.NewHandler(feishu.HandlerConfig{
		Verify: verifyCfg,
		OnMessage: func(m *channel.IncomingMessage) error {
			// 异步投递：**立即返回**，不阻塞 HTTP 响应。
			//
			// agent 跑一轮要数秒，远超飞书的事件响应窗口。同步处理会让
			// 端点超时 → 平台重投 → 重复处理。正确形态是先回 200 再后台处理。
			//
			// 队列满时返回错误 → 端点回 500 → 平台重试。这比无界队列
			// 吃光内存要好（见 server.Dispatcher 的文档）。
			if err := dispatcher.Enqueue(m); err != nil {
				return err
			}
			return nil
		},
	})

	mux := http.NewServeMux()
	mux.Handle(h.Path(), h)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// 飞书事件体很小，限制请求体防内存耗尽。
		ReadTimeout: 30 * time.Second,
	}

	ctx, cancel := signalContext()
	defer cancel()

	// 启动后即打印端点，让运维能确认「监听在哪里」而不是靠猜。
	fmt.Fprintf(os.Stderr, "taiji serve: 监听 %s，webhook 路径 %s\n", *addr, h.Path())

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		if err != nil {
			fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
			return 1
		}
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\ntaiji serve: 收到中断信号，正在关闭…")
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		// 顺序：先停 HTTP（不再收新请求），再停分发器（等在途消息处理完）。
		// 反过来的话，Shutdown 期间到达的请求会被投进已关闭的队列。
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "taiji serve: 关闭失败: %v\n", err)
			return 1
		}
		// HTTP 已停，等在途消息处理完再退出——否则已回 200 的消息被静默丢弃。
		dispatcher.Stop()
		fmt.Fprintln(os.Stderr, "taiji serve: 已关闭")
	}
	return 0
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
	sender, err := feishu.NewSender(feishu.SenderConfigFromEnv(loaded))
	if err != nil {
		bootstrap.CloseMCPSets(toolSets)
		return nil, err
	}

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
	executor, err := chat.NewExecutor(chat.Options{
		Config:     modelCfg,
		AppName:    "taiji",
		UserID:     "feishu",
		ToolSets:   toolSets,
		AllowTools: allowTools,
		Echo:       logw,
	})
	if err != nil {
		bootstrap.CloseMCPSets(toolSets)
		return nil, fmt.Errorf("装配执行器: %w", err)
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

// envMCPSpecs 从环境读 MCP server 配置（分号分隔的 name=command 列表）。
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
