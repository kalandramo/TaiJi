package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
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

// ===== 4. CLI 路径也认环境变量 =====

// CLI 路径必须与 serve 路径共用同一套 MCP 配置面。
//
// 历史缺口：envMCPSpecs 只接在 serve 路径，CLI 只认 --mcp flag。
// 后果是「设了 TAIJI_MCP_SERVERS 却在 taiji chat 里不生效」——
// 无报错、无提示，工具静默不可用。
func TestRunChat_ReadsMCPFromEnv(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	start := indexOf(text, "func runChat(")
	if start < 0 {
		t.Fatal("runChat not found")
	}
	end := indexOf(text[start:], "\nfunc ")
	if end < 0 {
		end = len(text) - start
	}
	body := text[start : start+end]

	if !contains(body, "envMCPSpecs()") {
		t.Error("runChat 未读取 TAIJI_MCP_SERVERS——" +
			"环境变量配了 MCP 却在 CLI 里静默失效")
	}
}

// 合并语义：环境变量在前、flag 在后，两者都要进配置。
func TestParseMCPSpecs_EnvAndFlagMerge(t *testing.T) {
	t.Setenv("TAIJI_MCP_SERVERS", "fromenv=echo env-cmd")

	// 模拟 runChat 的合并：env + flag
	specs := append(envMCPSpecs(), "fromflag=echo flag-cmd")
	cfgs, err := parseMCPSpecs(specs)
	if err != nil {
		t.Fatalf("parseMCPSpecs: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("got %d configs, want 2（env + flag）", len(cfgs))
	}
	names := []string{cfgs[0].Name, cfgs[1].Name}
	if names[0] != "fromenv" || names[1] != "fromflag" {
		t.Errorf("names = %v, want [fromenv fromflag]", names)
	}
}

// 远程 server 从环境变量带认证头（stdio 不带——子进程无 HTTP 头概念）。
func TestParseMCPSpecs_RemoteFromEnvCarriesAuth(t *testing.T) {
	t.Setenv("TAIJI_MCP_SERVERS", "remotesrv=https://api.example.com/mcp")
	t.Setenv("TAIJI_MCP_HEADERS_remotesrv", "Authorization:Bearer env-token")

	cfgs, err := parseMCPSpecs(envMCPSpecs())
	if err != nil {
		t.Fatalf("parseMCPSpecs: %v", err)
	}
	if len(cfgs) != 1 {
		t.Fatalf("got %d configs, want 1", len(cfgs))
	}
	if cfgs[0].Headers["Authorization"] != "Bearer env-token" {
		t.Errorf("远程 server 应带认证头，got %v", cfgs[0].Headers)
	}
}

// stdio server 不应带 HTTP 认证头——认证头只对远程有意义。
// 这条锁住「不给本地子进程塞无意义的头」。
func TestParseMCPSpecs_StdioIgnoresAuthHeaders(t *testing.T) {
	t.Setenv("TAIJI_MCP_HEADERS_localsrv", "Authorization:Bearer nope")

	cfgs, err := parseMCPSpecs([]string{"localsrv=echo hi"})
	if err != nil {
		t.Fatalf("parseMCPSpecs: %v", err)
	}
	if len(cfgs[0].Headers) != 0 {
		t.Errorf("stdio server 不应有 Headers，got %v", cfgs[0].Headers)
	}
}

// CLI 路径的白名单也必须认环境变量（与 serve 路径一致）。
//
// 历史缺口：TAIJI_ALLOW_TOOLS 只接在 serve 路径，CLI 只认 --allow-tool。
// 后果：设了环境变量却在 CLI 里显示「白名单为空」——工具静默不可用。
func TestRunChat_ReadsAllowToolsFromEnv(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	start := indexOf(text, "func runChat(")
	if start < 0 {
		t.Fatal("runChat not found")
	}
	end := indexOf(text[start:], "\nfunc ")
	if end < 0 {
		end = len(text) - start
	}
	body := text[start : start+end]

	if !contains(body, "envAllowTools()") {
		t.Error("runChat 未读取 TAIJI_ALLOW_TOOLS——" +
			"环境变量配了白名单却在 CLI 里静默失效")
	}
}

// ===== 6. 用户级权限表解析（方案 A）=====

func TestEnvPermissions_AbsentIsNil(t *testing.T) {
	t.Setenv("TAIJI_USER_PERMISSIONS", "")
	if got := envPermissions(); got != nil {
		t.Errorf("未配置时应返回 nil（不做用户级判定），got %v", got)
	}
}

func TestEnvPermissions_ParsesEntries(t *testing.T) {
	t.Setenv("TAIJI_USER_PERMISSIONS",
		"ws1:feishu:ou_alice=mockmcp_echo,infraverse_*;ws1:feishu:ou_admin=*")

	src := envPermissions()
	if src == nil {
		t.Fatal("应返回权限源")
	}

	ctx := context.Background()
	cases := []struct {
		principal string
		tool      string
		want      bool
	}{
		{"ws1:feishu:ou_alice", "mockmcp_echo", true},
		{"ws1:feishu:ou_alice", "infraverse_dce_ip", true},
		{"ws1:feishu:ou_alice", "other_tool", false},
		{"ws1:feishu:ou_admin", "anything", true},
		{"ws1:feishu:ou_unknown", "mockmcp_echo", false},
	}
	for _, c := range cases {
		got, err := src.Allowed(ctx, authz.AccessRequest{Principal: authz.Principal{Type: "im_user", ID: c.principal}, Action: c.tool})
		if err != nil {
			t.Fatalf("Allowed(%s,%s): %v", c.principal, c.tool, err)
		}
		if got != c.want {
			t.Errorf("Allowed(%s, %s) = %v, want %v", c.principal, c.tool, got, c.want)
		}
	}
}

func TestEnvPermissions_SkipsMalformedEntries(t *testing.T) {
	// 无 = 的条目跳过，不影响合法条目（一处笔误不导致启动失败）
	t.Setenv("TAIJI_USER_PERMISSIONS", "garbage;ws1:feishu:ou_alice=mockmcp_echo")
	src := envPermissions()
	if src == nil {
		t.Fatal("合法条目应被解析")
	}
	ok, _ := src.Allowed(context.Background(),
		authz.AccessRequest{Principal: authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_alice"}, Action: "mockmcp_echo"})
	if !ok {
		t.Error("合法条目应生效")
	}
}

func TestEnvPermissions_WhitespaceTolerated(t *testing.T) {
	t.Setenv("TAIJI_USER_PERMISSIONS", "  ws1:feishu:ou_alice = mockmcp_echo , mockmcp_other  ")
	src := envPermissions()
	ok, _ := src.Allowed(context.Background(),
		authz.AccessRequest{Principal: authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_alice"}, Action: "mockmcp_echo"})
	if !ok {
		t.Error("应容忍空白")
	}
	ok2, _ := src.Allowed(context.Background(),
		authz.AccessRequest{Principal: authz.Principal{Type: "im_user", ID: "ws1:feishu:ou_alice"}, Action: "mockmcp_other"})
	if !ok2 {
		t.Error("第二个工具也应生效")
	}
}
