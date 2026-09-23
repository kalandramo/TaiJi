package config

import (
	"strings"
	"testing"
)

// 安全不变量：工作区可控的环境不能覆盖保留键。
// 依据：设计文档 §4.6（NFR-9）——agent 可写工作区文件，若控制值可被覆盖，
// agent 就能通过写文件提权（改沙箱策略 / 模型路由 / 凭据引用）。

func TestMergeWorkspaceEnv_ReservedKeyIsSkipped(t *testing.T) {
	base := map[string]string{"MODEL_ROUTE": "trusted-model"}
	workspace := map[string]string{"MODEL_ROUTE": "evil-model"}

	var warnings []string
	got := MergeWorkspaceEnv(base, workspace, func(m string) { warnings = append(warnings, m) })

	if got["MODEL_ROUTE"] != "trusted-model" {
		t.Errorf("MODEL_ROUTE = %q, want %q (workspace must not override reserved key)",
			got["MODEL_ROUTE"], "trusted-model")
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "MODEL_ROUTE") {
		t.Errorf("warning %q should name the skipped key", warnings[0])
	}
}

func TestMergeWorkspaceEnv_ReservedKeyAbsentFromBaseStaysAbsent(t *testing.T) {
	// 保留键只能来自 base。base 没有时，工作区提供的也不得进入结果。
	got := MergeWorkspaceEnv(
		map[string]string{},
		map[string]string{"SANDBOX_POLICY": "none"},
		nil,
	)
	if _, present := got["SANDBOX_POLICY"]; present {
		t.Errorf("SANDBOX_POLICY = %q, want absent (reserved keys come only from base)", got["SANDBOX_POLICY"])
	}
}

func TestMergeWorkspaceEnv_NonReservedKeyPassesThrough(t *testing.T) {
	got := MergeWorkspaceEnv(
		map[string]string{"KEEP": "base"},
		map[string]string{"EXTRA": "ok"},
		nil,
	)
	if got["KEEP"] != "base" {
		t.Errorf("KEEP = %q, want base", got["KEEP"])
	}
	if got["EXTRA"] != "ok" {
		t.Errorf("EXTRA = %q, want ok", got["EXTRA"])
	}
}

func TestMergeWorkspaceEnv_NonReservedValueIsSanitized(t *testing.T) {
	got := MergeWorkspaceEnv(
		map[string]string{},
		map[string]string{"EXTRA": "a\x00b\x1bc"},
		nil,
	)
	if got["EXTRA"] != "abc" {
		t.Errorf("EXTRA = %q, want %q (control chars stripped)", got["EXTRA"], "abc")
	}
}

func TestMergeWorkspaceEnv_BaseIsNotMutated(t *testing.T) {
	base := map[string]string{"MODEL_ROUTE": "trusted"}
	MergeWorkspaceEnv(base, map[string]string{"MODEL_ROUTE": "evil", "X": "y"}, nil)
	if base["MODEL_ROUTE"] != "trusted" {
		t.Errorf("base mutated: MODEL_ROUTE = %q", base["MODEL_ROUTE"])
	}
	if _, leaked := base["X"]; leaked {
		t.Error("base mutated: workspace key X leaked into base")
	}
}

func TestSanitizeValue_StripsControlChars(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"a\x00b", "ab"},   // NUL
		{"a\x1bb", "ab"},   // ESC
		{"a\nb", "ab"},     // LF（防多行注入）
		{"a\r\nb", "ab"},   // CRLF
		{"a\tb", "a\tb"},   // tab 保留
		{"plain", "plain"}, // 无控制字符
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeValue(c.in); got != c.want {
			t.Errorf("sanitizeValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReservedKeys_ContainsDocumentedSet(t *testing.T) {
	// 设计文档 §4.6 列出的六个键必须在集合里。
	for _, k := range []string{
		"MODEL_ROUTE", "EXECUTION_MODE", "SANDBOX_POLICY",
		"CREDENTIAL_REF", "MCP_SERVERS", "TOOL_POLICY",
	} {
		if _, ok := ReservedKeys[k]; !ok {
			t.Errorf("ReservedKeys missing documented key %q", k)
		}
	}
}
