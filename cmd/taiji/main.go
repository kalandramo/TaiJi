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
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/kalandramo/TaiJi/internal/config"
)

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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	loaded, err := loadConfig(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji chat: %v\n", err)
		return 1
	}
	// 打印控制值生效情况（demo path：证明工作区未能覆盖保留键）
	printControlValues(loaded)
	// 实际对话循环由 Issue #2 实现。
	fmt.Fprintln(os.Stderr, "taiji chat: 骨架就绪（对话循环待 Issue #2 实现）")
	return 0
}

func runServe(args []string) int {
	fs, cfg := newFlagSet("serve", "启动渠道服务")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	loaded, err := loadConfig(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taiji serve: %v\n", err)
		return 1
	}
	printControlValues(loaded)
	// 实际服务由 Issue #5/#9 实现。
	fmt.Fprintln(os.Stderr, "taiji serve: 骨架就绪（渠道服务待 Issue #5 实现）")
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
