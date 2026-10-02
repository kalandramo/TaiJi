package cmd

import (
	"strings"
	"testing"
)

// /whoami 的输出契约（2026-10-02）。
//
// 用途：用户要能从这里拿到**可直接粘贴进 TAIJI_RBAC 的主体 ID**，
// 否则权限配置只能靠猜（日志里是脱敏摘要，不是可配置的原值）。

func TestWhoami_ShowsPrincipalIDAndConfigLocation(t *testing.T) {
	req := Request{
		PrincipalID:    "default:feishu:fbc51fde",
		IdentitySource: "union_id",
		OpenID:         "ou_2fab4154",
		Agent:          "bill",
	}
	out := whoamiText(req)

	// 核心：主体 ID 必须完整可见（用户要复制它）。
	if !strings.Contains(out, "default:feishu:fbc51fde") {
		t.Fatalf("主体 ID 未出现（用户无法复制）: %q", out)
	}
	// 必须告诉用户写到哪。
	if !strings.Contains(out, "TAIJI_RBAC") {
		t.Errorf("缺少配置位置提示: %q", out)
	}
	// 多 agent 部署下要知道本条消息属于哪个 agent。
	if !strings.Contains(out, "bill") {
		t.Errorf("缺少 agent 名: %q", out)
	}
	// open_id 用于排查「哪个应用」。
	if !strings.Contains(out, "ou_2fab4154") {
		t.Errorf("缺少 open_id: %q", out)
	}
}

func TestWhoami_UnionIDSaysConfigureOnce(t *testing.T) {
	// union_id 来源时，必须明确告知「配置一次即可」——
	// 这是该字段的核心价值，用户需要据此决定配置策略。
	out := whoamiText(Request{
		PrincipalID:    "default:feishu:on_x",
		IdentitySource: "union_id",
	})
	if !strings.Contains(out, "union_id") {
		t.Errorf("应说明身份键来源为 union_id: %q", out)
	}
	if !strings.Contains(out, "一次") {
		t.Errorf("union_id 时应提示只需配置一次: %q", out)
	}
	// 不应出现 open_id 的跨应用警告（那会误导）。
	if strings.Contains(out, "不跨应用") {
		t.Errorf("union_id 时不应出现跨应用警告: %q", out)
	}
}

func TestWhoami_OpenIDWarnsAboutCrossApp(t *testing.T) {
	// open_id 来源时必须给出跨应用警告——否则用户会以为
	// 「配一次到处能用」，然后在第二个 bot 上困惑于权限失效。
	out := whoamiText(Request{
		PrincipalID:    "default:feishu:ou_x",
		IdentitySource: "open_id",
	})
	if !strings.Contains(out, "不跨应用") {
		t.Errorf("open_id 时应警告不跨应用: %q", out)
	}
	if !strings.Contains(out, "每个") {
		t.Errorf("open_id 时应提示需逐应用配置: %q", out)
	}
}

func TestWhoami_EmptyPrincipalDoesNotInventIdentity(t *testing.T) {
	// 无身份时**不得编造** ID——那会让用户配置一个永不匹配的值。
	// 必须说明后果（按用户判定的权限一律不生效）。
	out := whoamiText(Request{})
	if strings.Contains(out, "default:") {
		t.Errorf("无身份时不应给出看起来可用的 ID: %q", out)
	}
	if !strings.Contains(out, "无法确定") {
		t.Errorf("应明确说明无法确定身份: %q", out)
	}
	if !strings.Contains(out, "不生效") {
		t.Errorf("应说明后果（权限不生效）: %q", out)
	}
}

func TestWhoami_RegisteredAndNotOwnerOnly(t *testing.T) {
	r, _ := newBuiltinRegistry(t)
	c, ok := r.Lookup("whoami")
	if !ok {
		t.Fatal("/whoami 未注册")
	}
	if c.OwnerOnly {
		t.Error("/whoami 不应限 owner——任何用户都该能查自己的身份")
	}
	if c.Handler == nil {
		t.Fatal("/whoami 缺 Handler")
	}
}

func TestWhoami_HandlerReturnsText(t *testing.T) {
	r, _ := newBuiltinRegistry(t)
	c, _ := r.Lookup("whoami")
	got, err := c.Handler(Request{PrincipalID: "ws:feishu:on_abc"})
	if err != nil {
		t.Fatalf("Handler 报错: %v", err)
	}
	if !strings.Contains(got, "ws:feishu:on_abc") {
		t.Errorf("Handler 输出应含主体 ID: %q", got)
	}
}
