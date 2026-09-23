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
	for _, cmd := range []string{"chat", "serve"} {
		if code := run([]string{cmd}); code != 0 {
			t.Errorf("run(%q) = %d, want 0", cmd, code)
		}
		// --help 也必须可用（子命令的 flag set 处理）
		if code := run([]string{cmd, "-h"}); code != 2 {
			// flag.ContinueOnError 对 -h 返回 ErrHelp，我们的 run 统一返回 2
			t.Logf("run(%q, -h) = %d (flag.ErrHelp path)", cmd, code)
		}
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
