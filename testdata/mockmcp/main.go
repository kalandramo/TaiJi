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
//	                          声明 readOnlyHint=true（只读，不改变外部状态）
//	write_note(note)        → 返回写入确认，**不声明注解**（默认视为写操作）
//
// 说明：MCP 工具名前缀 {server}_{tool} 由客户端（trpc-agent-go 的 WithName）
// 施加，server 端只声明裸工具名。多 server 场景下两个 server 都叫 echo，
// 客户端看到的会是 {srvA}_echo 与 {srvB}_echo——这正是 issue #3 AC-2 要验证的。
//
// 两个工具的注解差异用于验证「上下文级降权」：echo 只读（任何来源可执行），
// write_note 是写操作（仅可写上下文可执行）。
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
		mcp.WithToolAnnotations(&mcp.ToolAnnotations{
			ReadOnlyHint: mcp.BoolPtr(true),
		}),
	)
	server.RegisterTool(echoTool, handleEcho)

	// write_note 是**写工具**：不声明 readOnlyHint，故框架默认视为写操作
	// （ToolMetadata.ReadOnly 零值为 false）。用于验证上下文级降权。
	writeTool := mcp.NewTool("write_note",
		mcp.WithDescription("Persist a note (mutates external state)."),
		mcp.WithString("note", mcp.Required(), mcp.Description("The note to persist")),
	)
	server.RegisterTool(writeTool, handleWriteNote)

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

// handleWriteNote 是写工具的 handler。返回值含 "Wrote:" 前缀，
// 便于测试断言「工具是否真的执行」。
func handleWriteNote(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	note := ""
	if v, ok := req.Params.Arguments["note"].(string); ok {
		note = v
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent(fmt.Sprintf("Wrote: %s", note)),
		},
	}, nil
}
