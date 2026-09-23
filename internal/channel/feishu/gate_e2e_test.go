package feishu

import (
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/channel"
)

// 端到端 Demo path 验收（issue #6）。
//
// 与单元测试的区别：这里用**真实飞书事件 JSON** 走
// ParseCallback → EvaluateGate → authz.RequireWritable 全链路，
// 验证各层接线正确，而非直接构造结构体。
//
// 放在 feishu 包内：跨包验收需要同时 import channel 与 authz，
// 而 channel 不依赖 feishu（反向依赖会成环）。

func groupEventJSON(text, mentionsJSON string) string {
	// mentions 可选：为空时整段省略（含其后的逗号），避免产生非法 JSON。
	mentionsPart := ""
	if mentionsJSON != "" {
		mentionsPart = `"mentions":` + mentionsJSON + `,`
	}
	return `{
		"header":{"token":"` + testToken + `","event_type":"im.message.receive_v1"},
		"event":{
			"message":{
				"message_id":"om_1","chat_id":"oc_group","chat_type":"group",
				"message_type":"text",
				"content":"{\"text\":\"` + text + `\"}",
				` + mentionsPart + `
				"create_time":"1"
			},
			"sender":{"sender_id":{"open_id":"ou_sender"}}
		}
	}`
}

func directEventJSON(text string) string {
	return `{
		"header":{"token":"` + testToken + `","event_type":"im.message.receive_v1"},
		"event":{
			"message":{
				"message_id":"om_dm","chat_id":"","chat_type":"p2p",
				"message_type":"text",
				"content":"{\"text\":\"` + text + `\"}",
				"create_time":"1"
			},
			"sender":{"sender_id":{"open_id":"ou_sender"}}
		}
	}`
}

// TestDemoPath_GateEndToEnd 复现 issue #6 的四条 Demo path。
func TestDemoPath_GateEndToEnd(t *testing.T) {
	const botID = "ou_bot"

	cases := []struct {
		name       string
		body       string
		botOpenID  string // 空 = 模拟未配置
		activation channel.ActivationMode
		audience   channel.AudienceMode
		owners     []string
		wantAllow  bool
		wantReason string
	}{
		{
			name:       "① botOpenID 缺失 + 群消息 → 拒绝（fail-closed）",
			body:       groupEventJSON("hi", `[{"id":{"open_id":"ou_bot"},"key":"@_user_1","name":"Taiji"}]`),
			botOpenID:  "", // 未配置
			activation: channel.ActivationWhenMentioned,
			audience:   channel.AudienceEveryone,
			wantAllow:  false,
			wantReason: channel.ReasonBotOpenIDMissing,
		},
		{
			name:       "② 文本含 @bot 但 mentions 元数据为空 → 拒绝",
			body:       groupEventJSON("@Taiji hi", ""), // 文本有 @，元数据无
			botOpenID:  botID,
			activation: channel.ActivationWhenMentioned,
			audience:   channel.AudienceEveryone,
			wantAllow:  false,
			wantReason: channel.ReasonNotMentioned,
		},
		{
			name:       "③ owner_only 且 sender 非 owner → 拒绝",
			body:       groupEventJSON("hi", `[{"id":{"open_id":"ou_bot"},"key":"@_user_1","name":"Taiji"}]`),
			botOpenID:  botID,
			activation: channel.ActivationWhenMentioned,
			audience:   channel.AudienceOwnerOnly,
			owners:     []string{"ou_someone_else"}, // sender 是 ou_sender，不在内
			wantAllow:  false,
			wantReason: channel.ReasonNotOwner,
		},
		{
			name:       "④ 私聊 + always → 放行",
			body:       directEventJSON("hi"),
			botOpenID:  botID,
			activation: channel.ActivationAlways,
			audience:   channel.AudienceEveryone,
			wantAllow:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 1) 真实解析
			msg, err := newTestSource().ParseCallback(postRequest(c.body))
			if err != nil {
				t.Fatalf("ParseCallback: %v", err)
			}
			if msg == nil {
				t.Fatal("ParseCallback returned nil message")
			}

			// 2) 真实门禁判定
			d := channel.EvaluateGate(channel.GateInput{
				Audience:   c.audience,
				Activation: c.activation,
				ChatType:   msg.ChatType,
				BotOpenID:  c.botOpenID,
				SenderID:   msg.UserID,
				Mentions:   msg.Mentions,
				Owners:     c.owners,
			})

			if d.Allow != c.wantAllow {
				t.Fatalf("allow = %v, want %v (reason=%q)", d.Allow, c.wantAllow, d.Reason)
			}
			if !c.wantAllow && d.Reason != c.wantReason {
				t.Errorf("reason = %q, want %q", d.Reason, c.wantReason)
			}

			// 3) 放行路径：注入 channel 上下文后，写操作必须被拒（AC-5）
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
	mentionMeta := `[{"id":{"open_id":"ou_bot"},"key":"@_user_1","name":"Taiji"}]`

	withMeta, err := newTestSource().ParseCallback(
		postRequest(groupEventJSON("@Taiji hi", mentionMeta)))
	if err != nil {
		t.Fatal(err)
	}
	withoutMeta, err := newTestSource().ParseCallback(
		postRequest(groupEventJSON("@Taiji hi", "")))
	if err != nil {
		t.Fatal(err)
	}

	base := channel.GateInput{
		Activation: channel.ActivationWhenMentioned,
		ChatType:   channel.ChatGroup,
		BotOpenID:  botID,
		Audience:   channel.AudienceEveryone,
	}

	a := base
	a.Mentions = withMeta.Mentions
	if d := channel.EvaluateGate(a); !d.Allow {
		t.Errorf("with mention metadata should allow, got reason=%q", d.Reason)
	}

	b := base
	b.Mentions = withoutMeta.Mentions
	if d := channel.EvaluateGate(b); d.Allow {
		t.Error("without mention metadata should reject (must not match on text)")
	}
	_ = strings.TrimSpace
}
