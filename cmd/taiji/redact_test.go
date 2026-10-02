package main

import (
	"bytes"
	"strings"
	"testing"
)

// 控制值打印的凭据脱敏。
//
// 背景（2026-10-02 真实泄露）：printControlValues 原样打印控制值，
// 而 TAIJI_AGENTS 的值内含每个 agent 的 app_secret——
//
//	生效的控制值（来自受信启动环境）:
//	  TAIJI_AGENTS=name=root,app_id=cli_xxx,app_secret=xcnabOIOaSY5...（明文）
//
// 它出现在启动日志（stderr）、CI 归档、以及用户贴给他人排障的片段里。
// 控制值来自受信环境、本就要打印（#1 demo path 的证据面），
// 所以修法是**脱敏而非不打印**。

func TestRedactControlValue_MasksSecretInAgentsList(t *testing.T) {
	in := "name=root,app_id=cli_aaa,app_secret=SUPERSECRET1;name=bill,app_id=cli_bbb,app_secret=SUPERSECRET2"
	out := redactControlValue("TAIJI_AGENTS", in)

	// 核心断言：两个 secret 的**原值**不得出现。
	if strings.Contains(out, "SUPERSECRET1") || strings.Contains(out, "SUPERSECRET2") {
		t.Fatalf("app_secret 未被脱敏: %q", out)
	}
	// 非敏感部分必须保留——否则日志失去排障价值（看不到配了哪些 agent）。
	for _, want := range []string{"name=root", "app_id=cli_aaa", "name=bill", "app_id=cli_bbb"} {
		if !strings.Contains(out, want) {
			t.Errorf("脱敏过度，丢失了 %q: %q", want, out)
		}
	}
	// 必须留下「这里原本有个 secret」的痕迹。
	if !strings.Contains(out, "app_secret=") {
		t.Errorf("应保留 app_secret= 键名（表明该项存在）: %q", out)
	}
}

func TestRedactControlValue_PlainSensitiveKeysMasked(t *testing.T) {
	// 单值型敏感键：整值脱敏。
	cases := []struct{ key, val string }{
		{"TAIJI_MODEL_API_KEY", "sk-realkey"},
		{"FEISHU_APP_SECRET", "realsecret"},
		{"TAIJI_MCP_HEADERS_INFRAVERSE", "Bearer realtoken"},
	}
	for _, c := range cases {
		out := redactControlValue(c.key, c.val)
		if strings.Contains(out, c.val) {
			t.Errorf("key=%s 的值未被脱敏: %q", c.key, out)
		}
	}
}

func TestRedactControlValue_NonSensitiveUnchanged(t *testing.T) {
	// 非敏感值零改动——脱敏不能影响排障（#1 demo path 的证据面）。
	cases := []struct{ key, val string }{
		{"TAIJI_MODEL_NAME", "deepseek-chat"},
		{"TAIJI_MODEL_BASE_URL", "https://api.example.com/v1"},
		{"TAIJI_RBAC", "role:op=cmd:help;user:default:feishu:ou_x=op"},
		{"TAIJI_FEISHU_BOT_OPEN_ID", "ou_bot_123"},
		{"TAIJI_INSTRUCTION", "你是助手"},
	}
	for _, c := range cases {
		if got := redactControlValue(c.key, c.val); got != c.val {
			t.Errorf("key=%s 非敏感值被改动: got=%q want=%q", c.key, got, c.val)
		}
	}
}

func TestRedactControlValue_EmptyValueUnchanged(t *testing.T) {
	if got := redactControlValue("TAIJI_AGENTS", ""); got != "" {
		t.Errorf("空值应原样返回，got=%q", got)
	}
}

func TestRedactControlValue_URLWithSecretQueryMasked(t *testing.T) {
	// 值里嵌凭据的形态（如带 token 的 URL）——按敏感字段名脱敏。
	in := "https://user:hunter2@host/path"
	out := redactControlValue("TAIJI_MODEL_BASE_URL", in)
	// 用户信息段（user:password@）应被脱敏，但主机名保留。
	if strings.Contains(out, "hunter2") {
		t.Errorf("URL 中的口令未脱敏: %q", out)
	}
	if !strings.Contains(out, "host") {
		t.Errorf("主机名应保留（排障需要看连的是哪台）: %q", out)
	}
}

// 验证**真实打印链**（printControlValuesTo），而不只是纯函数。
//
// 理由：脱敏函数正确但没接上打印，等于没修——这类「接线缺口」
// 只有走完整调用链的测试才抓得到。
func TestPrintControlValuesTo_RedactsInRealOutput(t *testing.T) {
	var buf bytes.Buffer
	printControlValuesTo(&buf, map[string]string{
		"TAIJI_AGENTS": "name=root,app_id=cli_r,app_secret=LEAKED_A;name=bill,app_id=cli_b,app_secret=LEAKED_B",
		"MODEL_ROUTE":  "default",
	})
	out := buf.String()

	if strings.Contains(out, "LEAKED_A") || strings.Contains(out, "LEAKED_B") {
		t.Fatalf("打印链未脱敏，secret 出现在输出中:\n%s", out)
	}
	// 证据面必须保留：能看到配了哪几个 agent、以及 MLA 路由值。
	if !strings.Contains(out, "cli_r") || !strings.Contains(out, "cli_b") {
		t.Errorf("app_id 应保留（排障需要看配了哪些 agent）:\n%s", out)
	}
	if !strings.Contains(out, "MODEL_ROUTE=default") {
		t.Errorf("非敏感控制值应原样打印（#1 demo path 证据面）:\n%s", out)
	}
}
