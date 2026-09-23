// mockmcp 是一个最小的 stdio MCP server，用于本地验证 taiji 的 MCP 工具调用链路，
// 不依赖任何外部服务。
//
// 用法：
//
//	go run ./testdata/mockmcp                 # 默认暴露 echo
//	go run ./testdata/mockmcp -tag alpha      # 工具名仍为 echo（server 名前缀由客户端加）
//
// 工具：
//
//	echo(message, prefix?) → 返回 "<prefix><message>"，默认 prefix="Echo: "
//
// 说明：MCP 工具名前缀 {server}_{tool} 由客户端（trpc-agent-go 的 WithName）
// 施加，server 端只声明裸工具名。多 server 场景下两个 server 都叫 echo，
// 客户端看到的会是 {srvA}_echo 与 {srvB}_echo——这正是 issue #3 AC-2 要验证的。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	mcp "trpc.group/trpc-go/trpc-mcp-go"
)

func main() {
	tag := flag.String("tag", "", "仅用于日志区分实例，不影响工具名")
	flag.Parse()

	// 日志走 stderr：stdout 是 MCP 协议通道，不能污染。
	log.SetOutput(os.Stderr)
	if *tag != "" {
		log.Printf("mockmcp starting (tag=%s)", *tag)
	}

	server := mcp.NewStdioServer("mockmcp", "1.0.0")

	echoTool := mcp.NewTool("echo",
		mcp.WithDescription("Echo the input message back, with an optional prefix."),
		mcp.WithString("message", mcp.Required(), mcp.Description("The message to echo")),
		mcp.WithString("prefix", mcp.Description("Optional prefix, default 'Echo: '")),
	)
	server.RegisterTool(echoTool, handleEcho)

	if err := server.Start(); err != nil {
		log.Fatalf("mockmcp server error: %v", err)
	}
}

func handleEcho(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	message := ""
	if v, ok := req.Params.Arguments["message"].(string); ok {
		message = v
	}
	prefix := "Echo: "
	if v, ok := req.Params.Arguments["prefix"].(string); ok && v != "" {
		prefix = v
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent(fmt.Sprintf("%s%s", prefix, message)),
		},
	}, nil
}
