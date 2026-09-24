package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/bootstrap"
	"github.com/kalandramo/TaiJi/internal/config"
)

// MCP 认证（静态 token/API key）测试。
//
// 背景：MCPServerConfig.Headers 一直存在且被透传到 SDK，但**配置面没有入口**
// ——parseMCPSpecs 只产 stdio 配置（本地子进程不需要认证），远程 MCP
// （sse/streamable）配不出来。等于认证能力事实上不可用。
//
// 本文件覆盖三层：
//   1. 解析：spec → 配置（transport/URL/Headers）
//   2. 安全：认证头只从进程环境读，工作区文件不可覆盖
//   3. 端到端：Headers 真的出现在 HTTP 请求里（对假 MCP server 实测）

// ===== 1. 解析层 =====

func TestParseMCPSpecs_StdioUnchanged(t *testing.T) {
	// 回归护栏：原有 stdio 形态不能因新增远程支持而破坏。
	cfgs, err := parseMCPSpecs([]string{"mockmcp=go run ./testdata/mockmcp"})
	if err != nil {
		t.Fatalf("parseMCPSpecs: %v", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("got %d configs, want 1", len(cfgs))
	}
	c := cfgs[0]
	if c.Name != "mockmcp" || c.Transport != "stdio" {
		t.Errorf("name/transport = %q/%q, want mockmcp/stdio", c.Name, c.Transport)
	}
	if c.Command != "go" || len(c.Args) != 2 || c.Args[0] != "run" {
		t.Errorf("command/args = %q/%v, want go/[run ./testdata/mockmcp]", c.Command, c.Args)
	}
	if c.URL != "" || len(c.Headers) != 0 {
		t.Errorf("stdio 配置不应有 URL/Headers，got %q/%v", c.URL, c.Headers)
	}
}

func TestParseMCPSpecs_RemoteHTTPS(t *testing.T) {
	cfgs, err := parseMCPSpecs([]string{"github=https://api.example.com/mcp"})
	if err != nil {
		t.Fatalf("parseMCPSpecs: %v", err)
	}
	c := cfgs[0]
	if c.Transport != "streamable" {
		t.Errorf("transport = %q, want streamable (非 /sse 后缀的默认)", c.Transport)
	}
	if c.URL != "https://api.example.com/mcp" {
		t.Errorf("URL = %q", c.URL)
	}
}

func TestParseMCPSpecs_RemoteSSE(t *testing.T) {
	cfgs, err := parseMCPSpecs([]string{"srv=http://host:9000/sse"})
	if err != nil {
		t.Fatalf("parseMCPSpecs: %v", err)
	}
	if cfgs[0].Transport != "sse" {
		t.Errorf("transport = %q, want sse (/sse 后缀)", cfgs[0].Transport)
	}
}

func TestParseMCPSpecs_SSEWithTrailingSlash(t *testing.T) {
	// "http://host/sse/" 也应识别为 sse（尾斜杠常见）
	cfgs, err := parseMCPSpecs([]string{"srv=http://host/sse/"})
	if err != nil {
		t.Fatalf("parseMCPSpecs: %v", err)
	}
	if cfgs[0].Transport != "sse" {
		t.Errorf("transport = %q, want sse", cfgs[0].Transport)
	}
}

func TestParseMCPSpecs_RejectsMissingEquals(t *testing.T) {
	if _, err := parseMCPSpecs([]string{"no-equals-sign"}); err == nil {
		t.Error("缺 = 的 spec 应报错")
	}
}

func TestParseMCPSpecs_RejectsEmptyCommand(t *testing.T) {
	if _, err := parseMCPSpecs([]string{"name="}); err == nil {
		t.Error("空 target 应报错")
	}
}

// ===== 2. 认证头解析与安全 =====

func TestMCPHeadersFor_ParsesBearer(t *testing.T) {
	t.Setenv("TAIJI_MCP_HEADERS_github", "Authorization:Bearer ghp_secret123")
	got := mcpHeadersFor("github")
	if got["Authorization"] != "Bearer ghp_secret123" {
		t.Errorf("Authorization = %q, want %q", got["Authorization"], "Bearer ghp_secret123")
	}
}

func TestMCPHeadersFor_MultipleHeaders(t *testing.T) {
	t.Setenv("TAIJI_MCP_HEADERS_srv", "Authorization:Bearer tok;X-Api-Key:k123")
	got := mcpHeadersFor("srv")
	if got["Authorization"] != "Bearer tok" {
		t.Errorf("Authorization = %q", got["Authorization"])
	}
	if got["X-Api-Key"] != "k123" {
		t.Errorf("X-Api-Key = %q", got["X-Api-Key"])
	}
}

func TestMCPHeadersFor_ColonInValuePreserved(t *testing.T) {
	// 值内含冒号（如 URL、时间戳）必须保留——只切第一个冒号。
	t.Setenv("TAIJI_MCP_HEADERS_srv", "X-Ref:https://example.com:8443/path")
	got := mcpHeadersFor("srv")
	if got["X-Ref"] != "https://example.com:8443/path" {
		t.Errorf("X-Ref = %q, want 完整值（含冒号）", got["X-Ref"])
	}
}

func TestMCPHeadersFor_AbsentIsNil(t *testing.T) {
	got := mcpHeadersFor("nonexistent-server-xyz")
	if got != nil {
		t.Errorf("未配置时应返回 nil，got %v", got)
	}
}

func TestMCPHeadersFor_SkipsMalformedEntries(t *testing.T) {
	// 无冒号的条目跳过，不影响合法条目（避免一处笔误导致启动失败）
	t.Setenv("TAIJI_MCP_HEADERS_srv", "garbage;Authorization:Bearer ok")
	got := mcpHeadersFor("srv")
	if got["Authorization"] != "Bearer ok" {
		t.Errorf("合法条目应保留，got %v", got)
	}
	if len(got) != 1 {
		t.Errorf("非法条目应被跳过，got %d 项", len(got))
	}
}

func TestMCPHeadersFor_PerServerIsolation(t *testing.T) {
	// 每个 server 的头互不串味——这是多 server 场景的正确性前提。
	t.Setenv("TAIJI_MCP_HEADERS_srvA", "Authorization:Bearer tokenA")
	t.Setenv("TAIJI_MCP_HEADERS_srvB", "Authorization:Bearer tokenB")

	if got := mcpHeadersFor("srvA")["Authorization"]; got != "Bearer tokenA" {
		t.Errorf("srvA = %q, want Bearer tokenA", got)
	}
	if got := mcpHeadersFor("srvB")["Authorization"]; got != "Bearer tokenB" {
		t.Errorf("srvB = %q, want Bearer tokenB", got)
	}
}

// 安全边界：认证头前缀必须受 config 层保护，工作区文件不可覆盖。
//
// 这是凭据劫持面：攻击者若能通过工作区文件改写 MCP token，
// 就能把 agent 的 MCP 调用导到自己的 server 上。
func TestMCPHeadersPrefix_ProtectedFromWorkspace(t *testing.T) {
	base := map[string]string{
		"TAIJI_MCP_HEADERS_srv": "Authorization:Bearer trusted",
	}
	workspace := map[string]string{
		"TAIJI_MCP_HEADERS_srv": "Authorization:Bearer attacker",
	}
	var warned []string
	got := config.MergeWorkspaceEnv(base, workspace, func(m string) { warned = append(warned, m) })

	if got["TAIJI_MCP_HEADERS_srv"] != "Authorization:Bearer trusted" {
		t.Errorf("工作区覆盖了认证头！got %q, want 受信值", got["TAIJI_MCP_HEADERS_srv"])
	}
	if len(warned) == 0 {
		t.Error("跳过保留键时应告警")
	}
}

func TestMCPHeadersPrefix_WorkspaceOnlyKeyIsSkipped(t *testing.T) {
	// 基线没有、工作区独有 → 也不能进来（否则可凭空注入凭据）
	got := config.MergeWorkspaceEnv(
		map[string]string{},
		map[string]string{"TAIJI_MCP_HEADERS_evil": "Authorization:Bearer x"},
		nil,
	)
	if _, ok := got["TAIJI_MCP_HEADERS_evil"]; ok {
		t.Error("工作区独有的认证头键也应被跳过")
	}
}

// ===== 3. 端到端：Headers 真的进了 HTTP 请求 =====

// 假 MCP server：记录收到的 Authorization 头，按 streamable HTTP 协议
// 回一个最小 initialize 响应。
//
// 为什么必须实测而非只测字段透传：Headers 从 MCPServerConfig 到
// http.Request 之间隔着 SDK 的 buildHTTPOptions（trpc-agent-go
// tool/mcp/toolset.go:293-301）——只有实测能证明它真的到达了线缆。
func TestMCPHeaders_ReachHTTPRequest(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		// 最小 JSON-RPC 响应（initialize）
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
			},
		})
	}))
	defer srv.Close()

	sets, err := bootstrap.NewMCPSets([]bootstrap.MCPServerConfig{{
		Name:      "fake",
		Transport: "streamable",
		URL:       srv.URL,
		Headers:   map[string]string{"Authorization": "Bearer e2e-token"},
	}})
	if err != nil {
		t.Fatalf("NewMCPSets: %v", err)
	}
	defer bootstrap.CloseMCPSets(sets)

	if gotAuth != "Bearer e2e-token" {
		t.Errorf("MCP server 收到的 Authorization = %q, want %q —— "+
			"认证头没有到达 HTTP 请求", gotAuth, "Bearer e2e-token")
	}
}

// 反面对照：不配 Headers 时，请求不应带 Authorization。
// 这条锁住「不意外注入凭据」。
func TestMCPHeaders_AbsentMeansNoAuthHeader(t *testing.T) {
	var gotAuth string
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "s")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
			},
		})
	}))
	defer srv.Close()

	sets, err := bootstrap.NewMCPSets([]bootstrap.MCPServerConfig{{
		Name:      "fake",
		Transport: "streamable",
		URL:       srv.URL,
		// 无 Headers
	}})
	if err != nil {
		t.Fatalf("NewMCPSets: %v", err)
	}
	defer bootstrap.CloseMCPSets(sets)

	if !hit {
		t.Skip("server 未被触达（SDK 可能延迟初始化），本断言不适用")
	}
	if gotAuth != "" {
		t.Errorf("未配 Headers 却带了 Authorization = %q", gotAuth)
	}
}

// 端到端串联：spec → 环境变量 → 配置，认证头完整贯通。
func TestParseMCPSpecs_RemoteWithAuthFromEnv(t *testing.T) {
	t.Setenv("TAIJI_MCP_HEADERS_github", "Authorization:Bearer ghp_fromenv")

	cfgs, err := parseMCPSpecs([]string{"github=https://api.example.com/mcp"})
	if err != nil {
		t.Fatalf("parseMCPSpecs: %v", err)
	}
	c := cfgs[0]
	if c.Transport != "streamable" {
		t.Errorf("transport = %q", c.Transport)
	}
	if c.Headers["Authorization"] != "Bearer ghp_fromenv" {
		t.Errorf("Headers = %v, want Authorization 来自环境变量", c.Headers)
	}
	// 值中不应有分隔符残留
	if strings.Contains(c.Headers["Authorization"], ";") {
		t.Error("分隔符 ; 未剥离")
	}
}
