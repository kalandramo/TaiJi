package feishu

import (
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/channel"
)

// 端到端 Demo path 验收（issue #6）。
//
// **驱动方式已从 webhook 改为直构 IncomingMessage**：原型只用长连接，
// webhook 实现（含 ParseCallback）已删除。长连接路径的消息由 SDK 回调
// 直接产出 IncomingMessage（见 longconn.go），故这里直接构造该结构——
// 这正是长连接的真实形态，比用 webhook 解析更贴近实际。
//
// 验证内容不变：EvaluateGate 六步判定 + authz.RequireWritable 的降权语义。

// groupMsg 构造群聊消息（长连接路径的产物形态）。
func groupMsg(text, senderID string, mentions []channel.Mention) *channel.IncomingMessage {
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    senderID,
		ChatID:    "oc_group",
		ChatType:  channel.ChatGroup,
		MessageID: "om_1",
		Content:   text,
		Mentions:  mentions,
		Meta: &channel.ChannelMessageMeta{
			Provider: string(channel.PlatformFeishu),
			ChatType: "group",
			MessageID: "om_1",
			Text:      text,
		},
	}
}

// directMsg 构造私聊消息。
func directMsg(text, senderID string) *channel.IncomingMessage {
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    senderID,
		ChatID:    "",
		ChatType:  channel.ChatDirect,
		MessageID: "om_dm",
		Content:   text,
		Meta: &channel.ChannelMessageMeta{
			Provider: string(channel.PlatformFeishu),
			ChatType: "p2p",
			MessageID: "om_dm",
			Text:      text,
		},
	}
}

// botMention 构造 @bot 的元数据条目。
func botMention(botOpenID string) []channel.Mention {
	return []channel.Mention{{OpenID: botOpenID, Key: "@_user_1", Name: "Taiji"}}
}

// TestDemoPath_GateEndToEnd 复现 issue #6 的四条 Demo path。
func TestDemoPath_GateEndToEnd(t *testing.T) {
	const botID = "ou_bot"

	cases := []struct {
		name       string
		msg        *channel.IncomingMessage
		botOpenID  string // 空 = 模拟未配置
		activation channel.ActivationMode
		audience   channel.AudienceMode
		owners     []string
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "① botOpenID 缺失 + 群消息 → 拒绝（fail-closed）",
			msg:        groupMsg("hi", "ou_sender", botMention(botID)),
			botOpenID:  "", // 未配置
			activation: channel.ActivationWhenMentioned,
			audience:   channel.AudienceEveryone,
			wantAllow:  false,
			wantReason: channel.ReasonBotOpenIDMissing,
		},
		{
			name:       "② 文本含 @bot 但 mentions 元数据为空 → 拒绝",
			msg:        groupMsg("@Taiji hi", "ou_sender", nil), // 文本有 @，元数据无
			botOpenID:  botID,
			activation: channel.ActivationWhenMentioned,
			audience:   channel.AudienceEveryone,
			wantAllow:  false,
			wantReason: channel.ReasonNotMentioned,
		},
		{
			name:       "③ owner_only 且 sender 非 owner → 拒绝",
			msg:        groupMsg("hi", "ou_sender", botMention(botID)),
			botOpenID:  botID,
			activation: channel.ActivationWhenMentioned,
			audience:   channel.AudienceOwnerOnly,
			owners:     []string{"ou_someone_else"}, // sender 是 ou_sender，不在内
			wantAllow:  false,
			wantReason: channel.ReasonNotOwner,
		},
		{
			name:       "④ 私聊 + always → 放行",
			msg:        directMsg("hi", "ou_sender"),
			botOpenID:  botID,
			activation: channel.ActivationAlways,
			audience:   channel.AudienceEveryone,
			wantAllow:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 真实门禁判定
			d := channel.EvaluateGate(channel.GateInput{
				Audience:   c.audience,
				Activation: c.activation,
				ChatType:   c.msg.ChatType,
				BotOpenID:  c.botOpenID,
				SenderID:   c.msg.UserID,
				Mentions:   c.msg.Mentions,
				Owners:     c.owners,
			})

			if d.Allow != c.wantAllow {
				t.Fatalf("allow = %v, want %v (reason=%q)", d.Allow, c.wantAllow, d.Reason)
			}
			if !c.wantAllow && d.Reason != c.wantReason {
				t.Errorf("reason = %q, want %q", d.Reason, c.wantReason)
			}

			// 放行路径：注入 channel 上下文后，写操作必须被拒（AC-5）
			if d.Allow {
				ctx := authz.WithContextKind(t.Context(), authz.KindChannel)
				if err := authz.RequireWritable(ctx); err == nil {
					t.Error("allowed path must yield read-only context (write should be denied)")
				}
			}
		})
	}
}

// TestDemoPath_MentionMetadataDrivesGate 证明门禁读的是元数据而非文本。
//
// 反例对照：同样"文本里有 @bot"，元数据存在则放行、元数据缺失则拒绝。
// 若实现用文本匹配，这两个用例会得到相同结论——那就暴露了误判。
func TestDemoPath_MentionMetadataDrivesGate(t *testing.T) {
	const botID = "ou_bot"

	base := channel.GateInput{
		Activation: channel.ActivationWhenMentioned,
		ChatType:   channel.ChatGroup,
		BotOpenID:  botID,
		Audience:   channel.AudienceEveryone,
	}

	// 同样文本 "@Taiji hi"，差异只在元数据
	withMeta := base
	withMeta.Mentions = botMention(botID)
	if d := channel.EvaluateGate(withMeta); !d.Allow {
		t.Errorf("with mention metadata should allow, got reason=%q", d.Reason)
	}

	withoutMeta := base
	withoutMeta.Mentions = nil
	if d := channel.EvaluateGate(withoutMeta); d.Allow {
		t.Error("without mention metadata should reject (must not match on text)")
	}
}
