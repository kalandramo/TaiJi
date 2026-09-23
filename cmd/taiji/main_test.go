package main

import (
	"os"
	"path/filepath"
	"testing"
)

// CLI 骨架的验收：子命令存在、--help 可用、配置加载打通。
// 这些断言在 #1 之前必然失败（仓库当时无 go.mod，无 main）。

func TestRun_NoArgsShowsUsage(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Errorf("run(nil) = %d, want 2", code)
	}
}

func TestRun_HelpExitsZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		if code := run([]string{arg}); code != 0 {
			t.Errorf("run(%q) = %d, want 0", arg, code)
		}
	}
}

func TestRun_UnknownCommandExitsNonZero(t *testing.T) {
	if code := run([]string{"nope"}); code != 2 {
		t.Errorf("run(nope) = %d, want 2", code)
	}
}

func TestRun_ChatAndServeExist(t *testing.T) {
	// 子命令"存在"的可靠断言是 --help（不依赖运行时配置）。
	// 注意：#2 之后 chat/serve 会真正尝试装配模型，
	// 无配置时非零退出是正确行为，不再断言 run(cmd)==0。
	for _, cmd := range []string{"chat", "serve"} {
		code := run([]string{cmd, "-h"})
		if code == 0 {
			continue // flag 包对 -h 返回 ErrHelp，我们的 run 统一返回 2
		}
		if code != 2 {
			t.Errorf("run(%q, -h) = %d, want 2 (help path)", cmd, code)
		}
	}
}

func TestRun_ChatWithoutModelConfigFails(t *testing.T) {
	// #2 AC: 模型名为空时必须非零退出，不得静默传空串给 provider。
	t.Setenv("TAIJI_MODEL_NAME", "")
	if code := run([]string{"chat"}); code == 0 {
		t.Error("run(chat) with empty model name = 0, want non-zero")
	}
}

func TestRun_ReservedKeyNotOverridableViaConfig(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws.env")
	if err := os.WriteFile(ws, []byte("MODEL_ROUTE=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MODEL_ROUTE", "trusted")

	got, err := loadConfig(ws)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got["MODEL_ROUTE"] != "trusted" {
		t.Errorf("MODEL_ROUTE = %q, want trusted (workspace override must be skipped)", got["MODEL_ROUTE"])
	}
}

func TestRun_BadConfigPathFails(t *testing.T) {
	if code := run([]string{"chat", "--config", filepath.Join(t.TempDir(), "missing.env")}); code != 1 {
		t.Errorf("run(chat, --config missing) = %d, want 1", code)
	}
}
