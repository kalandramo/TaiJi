package feishu

import (
	"testing"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"

	"github.com/kalandramo/TaiJi/internal/channel"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// 身份键的跨应用稳定性（2026-10-02 实测暴露）。
//
// ## 问题
//
// 真实双 agent 同群运行：同一用户 @ 两个 bot，产生两个不同的 Principal。
//
//	[perm] denied:  principal=default:feishu:992b6d40   ← bill 应用
//	[perm] allowed: principal=default:feishu:a701f26e   ← root 应用
//
// 根因：飞书 open_id 是**应用维度**的（app-scoped），同一人在每个应用下不同。
// 而 RBAC 以 Principal.ID（内嵌 open_id）为键，故只在一个应用下配的权限
// 对另一个应用无效。
//
// 这不是新问题——docs/01-需求文档.md:946 早已把「飞书 open_id 的稳定性
// （跨应用/跨版本是否一致）」列为**未实测的假设**。本次实测推翻了它。
//
// ## 实测数据（探针输出，2026-10-02 13:39）
//
//	app_id=cli_aa0110cf25f41be2  open_id=2fab4154  union_id=fbc51fde
//	app_id=cli_aa126d218d789d05  open_id=c7b7e562  union_id=fbc51fde  ← 相同
//
// 结论：union_id 跨应用稳定，可作身份键；open_id 只能用于出站投递。

// 核心不变量：同一用户在不同应用下的消息，必须产出**同一个**身份键。
func TestIncomingFromLongConnEvent_UnionIDIsStableAcrossApps(t *testing.T) {
	// 同一用户的两次投递：不同 app、不同 open_id、相同 union_id。
	msgFromRoot := incomingFromLongConnEvent(mkEvent(
		"cli_root", "ou_open_in_root_app", "on_stable_union", "uid_same"))
	msgFromBill := incomingFromLongConnEvent(mkEvent(
		"cli_bill", "ou_open_in_bill_app", "on_stable_union", "uid_same"))

	if msgFromRoot == nil || msgFromBill == nil {
		t.Fatal("两个事件都应解析出消息")
	}

	// 身份键必须相同——这是 RBAC 只绑一次的前提。
	if msgFromRoot.IdentityID() != msgFromBill.IdentityID() {
		t.Fatalf("同一用户的身份键应跨应用相同：root=%q bill=%q\n"+
			"（若这里不等，RBAC 需要在每个应用下各绑一次）",
			msgFromRoot.IdentityID(), msgFromBill.IdentityID())
	}

	// open_id 必须保留（出站投递要用它，飞书发消息 API 只认 open_id）。
	if msgFromRoot.UserID != "ou_open_in_root_app" {
		t.Errorf("open_id 应原样保留在 UserID（出站要用），got=%q", msgFromRoot.UserID)
	}
	if msgFromBill.UserID != "ou_open_in_bill_app" {
		t.Errorf("open_id 应原样保留，got=%q", msgFromBill.UserID)
	}
}

// union_id 缺失时回退到 open_id（并保持可用）——不能因为缺字段就丢消息。
//
// 回退是有代价的（跨应用不再统一），所以调用方需要能区分——见
// IdentitySource 的用途：/whoami 据此提示用户「你的身份键可能不跨应用」。
func TestIncomingFromLongConnEvent_FallsBackToOpenIDWhenUnionMissing(t *testing.T) {
	msg := incomingFromLongConnEvent(mkEvent("cli_root", "ou_only_open", "", ""))
	if msg == nil {
		t.Fatal("union_id 缺失不应导致消息被丢弃")
	}
	if msg.IdentityID() != "ou_only_open" {
		t.Errorf("无 union_id 时应回退到 open_id，got=%q", msg.IdentityID())
	}
	if msg.IdentitySource != channel.IdentitySourceOpenID {
		t.Errorf("应标记来源为 open_id（回退），got=%q", msg.IdentitySource)
	}
}

// union_id 存在时必须优先用它，且标记来源。
func TestIncomingFromLongConnEvent_PrefersUnionID(t *testing.T) {
	msg := incomingFromLongConnEvent(mkEvent("cli_root", "ou_x", "on_y", "uid_z"))
	if msg.IdentityID() != "on_y" {
		t.Errorf("有 union_id 时应优先用它，got=%q", msg.IdentityID())
	}
	if msg.IdentitySource != channel.IdentitySourceUnionID {
		t.Errorf("应标记来源为 union_id，got=%q", msg.IdentitySource)
	}
}

// 两个身份字段都空 → 丢弃消息（fail-closed，与既有取向一致）。
func TestIncomingFromLongConnEvent_NoIdentityIsDropped(t *testing.T) {
	if msg := incomingFromLongConnEvent(mkEvent("cli_root", "", "", "")); msg != nil {
		t.Fatalf("无任何身份字段应丢弃消息，却得到 %+v", msg)
	}
}

// mkEvent 构造带指定身份字段的长连接事件。
func mkEvent(appID, openID, unionID, userID string) *larkim.P2MessageReceiveV1 {
	chatID, chatType, msgID, content := "oc_x", "group", "om_x", `{"text":"hi"}`
	sid := &larkim.UserId{}
	if openID != "" {
		sid.OpenId = &openID
	}
	if unionID != "" {
		sid.UnionId = &unionID
	}
	if userID != "" {
		sid.UserId = &userID
	}
	return &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{AppID: appID},
		},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: sid},
			Message: &larkim.EventMessage{
				ChatId: &chatID, ChatType: &chatType,
				MessageId: &msgID, Content: &content,
			},
		},
	}
}

// 群聊 @bot 的正文形态（2026-10-02 真实故障的**上游一环**）。
//
// 为什么要这条：下游（pipeline）剥前导 mention 依赖两个事实——
//
//	① Content 里保留了占位符（如 "@_user_1 /whoami"）
//	② Mentions[].Key 与正文里的占位符**字面相同**（"@_user_1"）
//
// 这两条以前只由我手填的测试数据"假定"，从未从 SDK 事件验证。
// 若任一条不成立，stripLeadingMentions 就会失效——
// 而且失效形态是静默的（命令又变成普通消息，模型编答案）。
func TestIncomingFromLongConnEvent_GroupMentionKeepsPlaceholderInContent(t *testing.T) {
	msg := incomingFromLongConnEvent(mkGroupEventWithMention(
		`{"text":"@_user_1 /whoami"}`, "@_user_1", "ou_bot"))

	if msg == nil {
		t.Fatal("应解析出消息")
	}
	// ① 正文保留占位符——否则下游无从判断是命令。
	if msg.Content != "@_user_1 /whoami" {
		t.Errorf("Content 应保留占位符，got=%q", msg.Content)
	}
	// ② Mentions 的 Key 与正文里的占位符字面一致——
	//    stripLeadingMentions 正是按 Key 精确匹配来剥的。
	if len(msg.Mentions) == 0 {
		t.Fatal("应解析出 mention")
	}
	if msg.Mentions[0].Key != "@_user_1" {
		t.Errorf("Mention.Key = %q, want %q", msg.Mentions[0].Key, "@_user_1")
		if msg.Mentions[0].Key != "@_user_1" {
			t.Log("Key 必须与正文占位符字面一致，否则下游无法精确剥离")
		}
	}
	// ③ 且正文确实以该 Key 开头（这是剥离生效的前提）。
	if !hasPrefix(msg.Content, msg.Mentions[0].Key) {
		t.Errorf("正文 %q 应以 Mention.Key %q 开头", msg.Content, msg.Mentions[0].Key)
	}
}

// mkGroupEventWithMention 构造带 @ 的群消息事件（SDK 真实结构）。
func mkGroupEventWithMention(contentJSON, mentionKey, botOpenID string) *larkim.P2MessageReceiveV1 {
	chatID, chatType, msgID, msgType := "oc_group", "group", "om_g", "text"
	openID, unionID := "ou_sender", "on_sender"
	key, name := mentionKey, "TaiJi"
	mType := "bot"
	return &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{AppID: "cli_root"},
		},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{
				OpenId: &openID, UnionId: &unionID,
			}},
			Message: &larkim.EventMessage{
				ChatId: &chatID, ChatType: &chatType,
				MessageId: &msgID, Content: &contentJSON, MessageType: &msgType,
				Mentions: []*larkim.MentionEvent{{
					Key: &key, Name: &name, MentionedType: &mType,
					Id: &larkim.UserId{OpenId: &botOpenID},
				}},
			},
		},
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
