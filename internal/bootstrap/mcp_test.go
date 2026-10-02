package bootstrap

import (
	"strings"
	"testing"
)

// MCP 装配契约（issue #3 AC）：
//   1. 空/非法配置必须报错（不得静默跳过）
//   2. 多 server 时工具名带前缀 {server}_{tool}，不冲突
//   3. server 启动失败 → Init 返回错误，装配层 fail-fast
//   4. stdio command 路径问题 → 错误信息能定位到路径（不是泛化的 "failed to initialize"）

func TestNewMCPSets_EmptyConfigIsRejected(t *testing.T) {
	// 一个 server 配置都没有时返回空集合，不报错（合法：无 MCP 也是有效配置）
	sets, err := NewMCPSets(nil)
	if err != nil {
		t.Errorf("NewMCPSets(nil) = error %v, want nil (no servers is valid)", err)
	}
	if len(sets) != 0 {
		t.Errorf("NewMCPSets(nil) returned %d sets, want 0", len(sets))
	}
}

func TestNewMCPSets_ServerWithoutNameIsRejected(t *testing.T) {
	_, err := NewMCPSets([]MCPServerConfig{{
		Transport: "stdio",
		Command:   "echo",
	}})
	if err == nil {
		t.Fatal("server without name should be rejected")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error %q should mention the missing name", err.Error())
	}
}

func TestNewMCPSets_ServerWithoutTransportIsRejected(t *testing.T) {
	_, err := NewMCPSets([]MCPServerConfig{{
		Name:    "srv",
		Command: "echo",
	}})
	if err == nil {
		t.Fatal("server without transport should be rejected")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("error %q should mention transport", err.Error())
	}
}

func TestNewMCPSets_StdioWithoutCommandIsRejected(t *testing.T) {
	_, err := NewMCPSets([]MCPServerConfig{{
		Name:      "srv",
		Transport: "stdio",
	}})
	if err == nil {
		t.Fatal("stdio server without command should be rejected")
	}
	if !strings.Contains(err.Error(), "command") {
		t.Errorf("error %q should mention command", err.Error())
	}
}

func TestNewMCPSets_HTTPWithoutURLIsRejected(t *testing.T) {
	_, err := NewMCPSets([]MCPServerConfig{{
		Name:      "srv",
		Transport: "streamable",
	}})
	if err == nil {
		t.Fatal("http server without url should be rejected")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error %q should mention url", err.Error())
	}
}

func TestNewMCPSets_DuplicateNameIsRejected(t *testing.T) {
	// 同名 server 会导致工具名前缀冲突（AC-2 的反面）
	_, err := NewMCPSets([]MCPServerConfig{
		{Name: "dup", Transport: "stdio", Command: "echo"},
		{Name: "dup", Transport: "stdio", Command: "echo"},
	})
	if err == nil {
		t.Fatal("duplicate server names should be rejected")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error %q should name the duplicate server", err.Error())
	}
}

func TestNewMCPSets_InitFailureNamesCommandPath(t *testing.T) {
	// AC-4: stdio command 指向不存在的路径时，错误必须包含该路径，
	// 而不是泛化的 "failed to initialize MCP tool set"。
	missing := "/nonexistent/path/to/mcp-server-binary"
	_, err := NewMCPSets([]MCPServerConfig{{
		Name:      "probe",
		Transport: "stdio",
		Command:   missing,
	}})
	if err == nil {
		t.Fatal("Init with nonexistent command should fail")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q must name the failing command path %q\n"+
			"（AC-4：路径问题必须可定位，不能是泛化的初始化失败）",
			err.Error(), missing)
	}
	if !strings.Contains(err.Error(), "probe") {
		t.Errorf("error %q should also name the server", err.Error())
	}
}

// ── NewMCPSetsTolerant：连接失败降级 / 配置错误仍 fail-fast ──
//
// 背景：serve 是长驻飞书 bot，一个外部 MCP 服务的网络抖动不该让 bot
// 完全起不来（真实故障：Infraverse SSE 不可达 → taiji serve 退出，
// 用户只看到 MCP 报错，无法区分「bot 坏了」与「工具服务临时不可达」）。
// 分层判据见 NewMCPSetsTolerant 的注释。

func TestNewMCPSetsTolerant_ConfigErrorStillFails(t *testing.T) {
	// 配置笔误不降级——越早暴露越好，且不会自愈。
	cases := []struct {
		name string
		cfg  MCPServerConfig
		want string
	}{
		{"缺 name", MCPServerConfig{Transport: "stdio", Command: "echo"}, "name"},
		{"缺 transport", MCPServerConfig{Name: "s", Command: "echo"}, "transport"},
		{"stdio 缺 command", MCPServerConfig{Name: "s", Transport: "stdio"}, "command"},
		{"http 缺 url", MCPServerConfig{Name: "s", Transport: "streamable"}, "url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := NewMCPSetsTolerant([]MCPServerConfig{tc.cfg})
			if err == nil {
				t.Fatalf("配置错误 %s 必须 fail-fast，不得降级", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q 应提到 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestNewMCPSetsTolerant_ConnectionFailureIsDegraded(t *testing.T) {
	// 连接失败 → 跳过并记录，**不返回 error**。
	missing := "/nonexistent/path/to/mcp-server-binary"
	sets, failed, err := NewMCPSetsTolerant([]MCPServerConfig{{
		Name:      "dead",
		Transport: "stdio",
		Command:   missing,
	}})
	if err != nil {
		t.Fatalf("连接失败不应 fail-fast（serve 路径），得到 error: %v", err)
	}
	if len(sets) != 0 {
		t.Errorf("失败的 server 不应产生 ToolSet，得到 %d 个", len(sets))
	}
	if len(failed) != 1 {
		t.Fatalf("应记录 1 个失败 server，得到 %d 个", len(failed))
	}
	if failed[0].Name != "dead" {
		t.Errorf("失败记录应含 server 名，得到 %q", failed[0].Name)
	}
	if failed[0].Err == nil {
		t.Error("失败记录应含原因（供调用方告警）")
	}
	if !strings.Contains(failed[0].Err.Error(), missing) {
		t.Errorf("失败原因应含命令路径（AC-4），得到 %q", failed[0].Err)
	}
}

func TestNewMCPSetsTolerant_PartialFailureKeepsHealthyServers(t *testing.T) {
	// 一个死、一个活的 → 活的那个仍可用（这是降级的全部意义）。
	sets, failed, err := NewMCPSetsTolerant([]MCPServerConfig{
		{Name: "dead", Transport: "stdio", Command: "/nonexistent/mcp-binary"},
		{Name: "alive", Transport: "stdio", Command: "go", Args: []string{"run", "../../testdata/mockmcp"}},
	})
	if err != nil {
		t.Fatalf("部分失败不应返回 error，得到: %v", err)
	}
	if len(failed) != 1 || failed[0].Name != "dead" {
		t.Errorf("应只记录 dead 失败，得到 %+v", failed)
	}
	if len(sets) != 1 {
		t.Fatalf("alive 应仍装配（降级不牵连健康 server），得到 %d 个", len(sets))
	}
	closeAll(sets)
}

func TestNewMCPSetsTolerant_EmptyConfigIsValid(t *testing.T) {
	sets, failed, err := NewMCPSetsTolerant(nil)
	if err != nil || len(sets) != 0 || len(failed) != 0 {
		t.Errorf("空配置应返回 (nil,nil,nil)，得到 (%v,%v,%v)", sets, failed, err)
	}
}
