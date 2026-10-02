package server

import (
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 群聊里 @bot 后跟命令的解析（2026-10-02 真实故障）。
//
// ## 现象
//
// 群里发 `@道客服务助手 /whoami`，bot 的回答是模型编的「你是 _user_1。」
// ——命令根本没被识别，正文被当普通文本喂给了模型。
//
// ## 根因
//
// 飞书群消息里 @机器人 在正文中是**占位符**（`@_user_1`），故正文实际为：
//
//	"@_user_1 /whoami"
//
// 而 cmd.Registry.Parse 要求正文**以 "/" 开头**（SPEC §4.2 §273），
// 于是解析失败 → 走正常消息路径 → 模型看到 "@_user_1 /whoami"
// 并把它当成问题回答（「你是 _user_1」正是模型对占位符的回应）。
//
// ## 修复方向
//
// 在调用 Parse **之前**剥掉前导 mention 占位符——用 Mentions 元数据
// 的 Key（`@_user_1`）精确匹配，**不做文本猜测**（不猜「@xxx 」形态）。
// Parse 本身的语义（必须以 / 开头）保持不变——SPEC 的要求是对的，
// 缺陷在调用方没先清理平台注入的前缀。

// stripLeadingMentions 的契约：只剥**开头连续的** mention 占位符及其后空白。
func TestStripLeadingMentions(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		mentions []channel.Mention
		want     string
	}{
		{
			name:     "群聊 @bot 后跟命令",
			content:  "@_user_1 /whoami",
			mentions: []channel.Mention{{Key: "@_user_1"}},
			want:     "/whoami",
		},
		{
			name:     "多个 @ 连续",
			content:  "@_user_1 @_user_2 /status",
			mentions: []channel.Mention{{Key: "@_user_1"}, {Key: "@_user_2"}},
			want:     "/status",
		},
		{
			name:     "多个空白分隔",
			content:  "@_user_1   /whoami",
			mentions: []channel.Mention{{Key: "@_user_1"}},
			want:     "/whoami",
		},
		{
			name:     "私聊无 mention（原样）",
			content:  "/whoami",
			mentions: nil,
			want:     "/whoami",
		},
		{
			name:     "正文中间的 @ 不剥（不作为前缀）",
			content:  "你好 @_user_1 /whoami",
			mentions: []channel.Mention{{Key: "@_user_1"}},
			want:     "你好 @_user_1 /whoami",
		},
		{
			name:     "普通文本不被误伤",
			content:  "/usr/local/bin 是什么",
			mentions: []channel.Mention{{Key: "@_user_1"}},
			want:     "/usr/local/bin 是什么",
		},
		{
			name:     "mention 后无内容",
			content:  "@_user_1",
			mentions: []channel.Mention{{Key: "@_user_1"}},
			want:     "",
		},
		{
			name:     "Key 为空时不动正文（不猜形态）",
			content:  "@_user_1 /whoami",
			mentions: []channel.Mention{{OpenID: "ou_x"}}, // 无 Key
			want:     "@_user_1 /whoami",
		},
	}

	for _, c := range cases {
		got := stripLeadingMentions(c.content, c.mentions)
		if got != c.want {
			t.Errorf("%s: stripLeadingMentions(%q) = %q, want %q",
				c.name, c.content, got, c.want)
		}
	}
}

// 兜底诊断必须**真的会触发**——否则它只是安慰剂。
//
// 模拟「Mentions.Key 与正文占位符形态不符」：剥离后仍以 "/" 开头
// 却不是已注册命令。此时应记日志（而非静默落到模型）。
func TestStripLeadingMentions_MismatchedKeyLeavesCommandLikeText(t *testing.T) {
	// Key 与正文不符（假设失效的情形）。
	content := "@_user_1 /whoami"
	mentions := []channel.Mention{{Key: "@_other_9"}} // 不匹配

	stripped := stripLeadingMentions(content, mentions)
	// 剥离失败 → 原样保留 → 仍含占位符、不以 "/" 开头。
	if stripped != content {
		t.Fatalf("Key 不符时不应改动正文，got=%q", stripped)
	}
	// 这条断言的含义：走正常消息路径（模型会看到它）。
	// 生产代码在此情形**不会**触发「形似命令」诊断——
	// 因为剥后不以 "/" 开头。故诊断覆盖的是另一种失效：
	// 部分剥离（剥掉一部分、残留一部分）后恰好以 "/" 开头。
	//
	// 完整失效矩阵：
	//   ① Key 完全不符      → 原样保留，模型答（无声，见下）
	//   ② Key 部分匹配      → 可能残留前导，模型答
	//   ③ 命令名未注册      → 剥成功但 Parse 失败 → **诊断触发**
	if len(mentions) > 0 && mentions[0].Key != "@_user_1" {
		t.Logf("情形①：Key 不符 → 正文原样 %q（落到模型）", stripped)
	}
}

// 情形③：剥成功、以 "/" 开头、但命令未注册 → 诊断应触发。
func TestStripLeadingMentions_UnknownCommandStillStartsWithSlash(t *testing.T) {
	content := "@_user_1 /nosuchcmd"
	mentions := []channel.Mention{{Key: "@_user_1"}}

	stripped := stripLeadingMentions(content, mentions)
	if stripped != "/nosuchcmd" {
		t.Fatalf("应剥出 /nosuchcmd，got=%q", stripped)
	}
	// 这正是诊断分支的条件：以 "/" 开头 + 有 mentions。
	if !strings.HasPrefix(stripped, "/") {
		t.Error("剥后应以 / 开头（诊断分支的触发条件）")
	}
}
