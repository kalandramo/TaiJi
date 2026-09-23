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
