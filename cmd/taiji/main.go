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
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kalandramo/TaiJi/internal/bootstrap"
	"github.com/kalandramo/TaiJi/internal/channel/feishu"
	"github.com/kalandramo/TaiJi/internal/chat"
	"github.com/kalandramo/TaiJi/internal/config"
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
		// #5 只做入站：解析出的消息由 handler 打印（Demo path 的证据面）。
		// OnMessage 留空——接入 agent 的端到端闭环由 #9 接线，
		// 此处不塞空实现（空回调会让「已接线」与「未接线」在代码上无法区分）。
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
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "taiji serve: 关闭失败: %v\n", err)
			return 1
		}
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
