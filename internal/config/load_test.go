package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_WorkspaceCannotOverrideReservedKey(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "workspace.env")
	// 工作区试图篡改模型路由与沙箱策略
	if err := os.WriteFile(ws, []byte("MODEL_ROUTE=evil-model\nSANDBOX_POLICY=none\nEXTRA=ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{"MODEL_ROUTE": "trusted-model", "SANDBOX_POLICY": "strict"}

	var warnings []string
	got, err := Load(env, ws, func(m string) { warnings = append(warnings, m) })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got["MODEL_ROUTE"] != "trusted-model" {
		t.Errorf("MODEL_ROUTE = %q, want trusted-model", got["MODEL_ROUTE"])
	}
	if got["SANDBOX_POLICY"] != "strict" {
		t.Errorf("SANDBOX_POLICY = %q, want strict", got["SANDBOX_POLICY"])
	}
	if got["EXTRA"] != "ok" {
		t.Errorf("EXTRA = %q, want ok (non-reserved key should pass)", got["EXTRA"])
	}
	if len(warnings) != 2 {
		t.Errorf("got %d warnings, want 2 (one per reserved key): %v", len(warnings), warnings)
	}
}

func TestLoad_NilEnvIsRejected(t *testing.T) {
	if _, err := Load(nil, "", nil); err == nil {
		t.Error("Load(nil, ...) should return an error, got nil")
	}
}

func TestLoad_EmptyWorkspacePathIsNoOp(t *testing.T) {
	env := map[string]string{"A": "1"}
	got, err := Load(env, "", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got["A"] != "1" {
		t.Errorf("A = %q, want 1", got["A"])
	}
}

func TestReadWorkspaceEnv_RejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(ws, []byte("NO_EQUALS_HERE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(map[string]string{}, ws, nil); err == nil {
		t.Error("malformed line should produce an error")
	}
}

func TestReadWorkspaceEnv_SkipsCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws.env")
	content := "# comment\n\nA=1\n  B = 2  \n"
	if err := os.WriteFile(ws, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(map[string]string{}, ws, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got["A"] != "1" || got["B"] != "2" {
		t.Errorf("got A=%q B=%q, want A=1 B=2", got["A"], got["B"])
	}
}
