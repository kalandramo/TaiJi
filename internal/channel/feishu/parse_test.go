package feishu

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 解析层的安全不变量（FR-10.2）：主体 ID 必须取自平台元数据，不由调用方参数决定。
// 依据：docs/03-原型设计文档.md:641（§4.4.5）。
//
// 参照实现的反例是 trpc-agent-go 的 preAuthIdentityMiddleware——从 HTTP header
// 直读 userID，任何调用方都能声称自己是任何人。本层的防线是：
// ParseCallback 只接收 *http.Request，没有「传入身份」的参数位。

// richEvent 构造含 mentions / 话题 / 多字段的飞书事件体。
type richEvent struct {
	token      string
	eventType  string
	openID     string
	userID     string // 用于证明「不得用 user_id 冒充 open_id」
	chatID     string
	chatType   string
	messageID  string
	text       string
	threadID   string
	rootID     string
	mentions   []mentionSpec
	msgType    string
	omitEvent  bool
	omitSender bool
}

type mentionSpec struct{ key, openID, name string }

func (e richEvent) json() string {
	if e.eventType == "" {
		e.eventType = "im.message.receive_v1"
	}
	if e.msgType == "" {
		e.msgType = "text"
	}
	// chatType 不做默认化：空 chat_type 是「未知值」用例要覆盖的输入，
	// 默认成 p2p 会让该用例测不到 normalizeChatType 的 fail-closed 方向。

	if e.omitEvent {
		// 无 event 字段的应用级通知事件（如菜单点击回调）。
		// 用途：验证「非消息事件返回 nil」。注意这与「消息事件缺 payload」
		// 是两种不同情形，后者必须报错——见 TestParseCallback_MessageEventMissingPayloadFails。
		return `{"schema":"2.0","header":{"event_id":"ev_1","token":"` + e.token +
			`","event_type":"application.bot.menu_v6"}}`
	}

	var b strings.Builder
	b.WriteString(`{"schema":"2.0","header":{"event_id":"ev_1","token":"` + e.token +
		`","event_type":"` + e.eventType + `"}`)

	b.WriteString(`,"event":{`)
	if !e.omitSender {
		b.WriteString(`"sender":{"sender_id":{"open_id":"` + e.openID +
			`","user_id":"` + e.userID + `"},"sender_type":"user"},`)
	}
	b.WriteString(`"message":{"message_id":"` + e.messageID +
		`","chat_id":"` + e.chatID +
		`","chat_type":"` + e.chatType +
		`","message_type":"` + e.msgType +
		`","content":` + contentJSON(e.msgType, e.text))
	if e.threadID != "" {
		b.WriteString(`,"thread_id":"` + e.threadID + `"`)
	}
	if e.rootID != "" {
		b.WriteString(`,"root_id":"` + e.rootID + `"`)
	}
	if len(e.mentions) > 0 {
		b.WriteString(`,"mentions":[`)
		for i, m := range e.mentions {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"key":"` + m.key + `","id":{"open_id":"` + m.openID +
				`"},"name":"` + m.name + `"}`)
		}
		b.WriteString(`]`)
	}
	b.WriteString(`}}}`)
	return b.String()
}

// contentJSON 生成飞书 message.content —— 它本身是一个 JSON 字符串。
func contentJSON(msgType, text string) string {
	if msgType != "text" {
		return `"{\"image_key\":\"img_1\"}"`
	}
	return `"{\"text\":\"` + text + `\"}"`
}

func newTestSource() *Source {
	return NewSource(VerifyConfig{VerificationToken: testToken, EncryptKey: testKey})
}

// --- 主体解析：取自 open_id，不由调用方决定 ---

func TestParseCallback_UserIDComesFromOpenID(t *testing.T) {
	ev := richEvent{
		token: testToken, openID: "ou_real_open_id", userID: "u_attacker_supplied",
		chatID: "oc_1", messageID: "om_1", text: "hello",
	}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if msg == nil {
		t.Fatal("ParseCallback = nil, want message")
	}
	if msg.UserID != "ou_real_open_id" {
		t.Errorf("UserID = %q, want %q (must come from open_id, not user_id)",
			msg.UserID, "ou_real_open_id")
	}
}

func TestParseCallback_PlatformIsFeishu(t *testing.T) {
	ev := richEvent{token: testToken, openID: "ou_1", chatID: "oc_1", messageID: "om_1", text: "hi"}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil || msg == nil {
		t.Fatalf("ParseCallback = %v, %v", msg, err)
	}
	if msg.Platform != channel.PlatformFeishu {
		t.Errorf("Platform = %q, want %q", msg.Platform, channel.PlatformFeishu)
	}
}

// --- 基本字段 ---

func TestParseCallback_BasicFields(t *testing.T) {
	ev := richEvent{
		token: testToken, openID: "ou_1", chatID: "oc_chat",
		messageID: "om_msg", text: "解析这段文本",
	}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil || msg == nil {
		t.Fatalf("ParseCallback = %v, %v", msg, err)
	}

	if msg.ChatID != "oc_chat" {
		t.Errorf("ChatID = %q, want oc_chat", msg.ChatID)
	}
	if msg.MessageID != "om_msg" {
		t.Errorf("MessageID = %q, want om_msg", msg.MessageID)
	}
	if msg.Content != "解析这段文本" {
		t.Errorf("Content = %q, want 解析这段文本", msg.Content)
	}
	if msg.Meta == nil {
		t.Fatal("Meta = nil, want populated meta")
	}
	if msg.Meta.Provider != "feishu" {
		t.Errorf("Meta.Provider = %q, want feishu", msg.Meta.Provider)
	}
}

// --- chat_type 归一化（fail-closed 方向） ---

func TestParseCallback_ChatTypeNormalization(t *testing.T) {
	cases := []struct {
		platform string
		want     channel.ChatType
	}{
		{"p2p", channel.ChatDirect},
		{"group", channel.ChatGroup},
		// 未知值必须归为 group：群聊要过 @ 门禁，私聊直接放行。
		// 把未知值当 direct 会绕过门禁——这是 fail-open 方向的错误。
		{"topic_group", channel.ChatGroup},
		{"", channel.ChatGroup},
	}
	for _, c := range cases {
		ev := richEvent{token: testToken, openID: "ou_1", chatID: "oc_1", messageID: "om_1",
			text: "hi", chatType: c.platform}
		msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
		if err != nil || msg == nil {
			t.Fatalf("chat_type=%q: ParseCallback = %v, %v", c.platform, msg, err)
		}
		if msg.ChatType != c.want {
			t.Errorf("chat_type=%q → ChatType = %q, want %q", c.platform, msg.ChatType, c.want)
		}
		if msg.Meta.ChatType != c.platform {
			t.Errorf("chat_type=%q → Meta.ChatType = %q, want platform raw value",
				c.platform, msg.Meta.ChatType)
		}
	}
}

// --- mentions：@ 判定用元数据不用文本 ---

func TestParseCallback_MentionsFromMetadata(t *testing.T) {
	ev := richEvent{
		token: testToken, openID: "ou_sender", chatID: "oc_1", messageID: "om_1",
		text: "@_user_1 你好", chatType: "group",
		mentions: []mentionSpec{
			{key: "@_user_1", openID: "ou_bot", name: "TaiJi"},
			{key: "@_user_2", openID: "ou_other", name: "Other"},
		},
	}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil || msg == nil {
		t.Fatalf("ParseCallback = %v, %v", msg, err)
	}

	if len(msg.Mentions) != 2 {
		t.Fatalf("got %d mentions, want 2", len(msg.Mentions))
	}
	if msg.Mentions[0].OpenID != "ou_bot" {
		t.Errorf("Mentions[0].OpenID = %q, want ou_bot", msg.Mentions[0].OpenID)
	}
	if msg.Mentions[0].Key != "@_user_1" {
		t.Errorf("Mentions[0].Key = %q, want @_user_1", msg.Mentions[0].Key)
	}
	if msg.Mentions[0].Name != "TaiJi" {
		t.Errorf("Mentions[0].Name = %q, want TaiJi", msg.Mentions[0].Name)
	}
}

func TestParseCallback_NoMentionsIsEmptyNotNil(t *testing.T) {
	// 门禁会遍历 Mentions。nil 与空切片遍历行为相同，但测试断言上
	// 用 len 更稳——这里锁住「不 panic」即可。
	ev := richEvent{token: testToken, openID: "ou_1", chatID: "oc_1", messageID: "om_1", text: "hi"}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil || msg == nil {
		t.Fatalf("ParseCallback = %v, %v", msg, err)
	}
	if len(msg.Mentions) != 0 {
		t.Errorf("Mentions = %v, want empty", msg.Mentions)
	}
}

// --- 话题元数据（供 #7 路由判定） ---

func TestParseCallback_ThreadContext(t *testing.T) {
	ev := richEvent{
		token: testToken, openID: "ou_1", chatID: "oc_1", messageID: "om_1",
		text: "hi", chatType: "group", threadID: "omt_thread", rootID: "om_root",
	}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil || msg == nil {
		t.Fatalf("ParseCallback = %v, %v", msg, err)
	}

	if msg.Meta.ThreadID != "omt_thread" {
		t.Errorf("Meta.ThreadID = %q, want omt_thread", msg.Meta.ThreadID)
	}
	if msg.Meta.RootID != "om_root" {
		t.Errorf("Meta.RootID = %q, want om_root", msg.Meta.RootID)
	}
	// 保守判定：有 thread_id 才算话题（§4.4.4）
	if msg.Meta.NativeContextType != "thread" {
		t.Errorf("Meta.NativeContextType = %q, want thread", msg.Meta.NativeContextType)
	}
}

func TestParseCallback_NonThreadHasNoContextType(t *testing.T) {
	ev := richEvent{token: testToken, openID: "ou_1", chatID: "oc_1", messageID: "om_1", text: "hi"}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil || msg == nil {
		t.Fatalf("ParseCallback = %v, %v", msg, err)
	}
	if msg.Meta.NativeContextType != "" {
		t.Errorf("Meta.NativeContextType = %q, want empty (no thread)", msg.Meta.NativeContextType)
	}
}

// --- 非消息事件返回 nil ---

func TestParseCallback_NonMessageEventReturnsNil(t *testing.T) {
	for _, et := range []string{
		"im.chat.disabled_v1",
		"im.message.reaction.created_v1",
		"application.bot.menu_v6",
	} {
		ev := richEvent{token: testToken, eventType: et, openID: "ou_1", chatID: "oc_1",
			messageID: "om_1", text: "hi"}
		msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
		if err != nil {
			t.Errorf("event_type=%q: ParseCallback error = %v, want nil", et, err)
		}
		if msg != nil {
			t.Errorf("event_type=%q: ParseCallback = %+v, want nil", et, msg)
		}
	}
}

func TestParseCallback_AppLevelEventWithoutPayloadReturnsNil(t *testing.T) {
	// 应用级通知事件没有 event 字段。它不是消息，返回 nil 而非报错。
	ev := richEvent{token: testToken, omitEvent: true}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil {
		t.Errorf("ParseCallback error = %v, want nil", err)
	}
	if msg != nil {
		t.Errorf("ParseCallback = %+v, want nil", msg)
	}
}

func TestParseCallback_MessageEventMissingPayloadFails(t *testing.T) {
	// 声明是消息事件（im.message.receive_v1）却没有 event.message ——
	// 这是畸形请求，必须报错。静默返回 nil 会让调用方以为「收到了一个
	// 非消息事件」而跳过，实际是有人构造了半截消息体。
	body := `{"schema":"2.0","header":{"event_id":"ev_1","token":"` + testToken +
		`","event_type":"im.message.receive_v1"},"event":{}}`
	if _, err := newTestSource().ParseCallback(postRequest(body)); err == nil {
		t.Fatal("ParseCallback on message event without payload = nil, want error")
	}
}

func TestParseCallback_MessageEventWithoutSenderFails(t *testing.T) {
	// 是消息事件却拿不到 sender —— 不能产出 UserID 为空的消息。
	// 空 UserID 会让下游 owner 比对失效（§4.4.5 的 fail-closed 要求）。
	ev := richEvent{token: testToken, omitSender: true, chatID: "oc_1", messageID: "om_1", text: "hi"}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err == nil && msg != nil && msg.UserID == "" {
		t.Fatal("message event without sender produced a message with empty UserID")
	}
}

// --- 加密事件 ---

func TestParseCallback_EncryptedEvent(t *testing.T) {
	ev := richEvent{token: testToken, openID: "ou_enc", chatID: "oc_enc", messageID: "om_enc", text: "加密文本"}
	body := `{"encrypt":"` + encryptFeishuBody(t, testKey, ev.json()) + `"}`

	msg, err := newTestSource().ParseCallback(postRequest(body))
	if err != nil {
		t.Fatalf("ParseCallback on encrypted event: %v", err)
	}
	if msg == nil {
		t.Fatal("ParseCallback = nil, want message")
	}
	if msg.UserID != "ou_enc" {
		t.Errorf("UserID = %q, want ou_enc", msg.UserID)
	}
	if msg.Content != "加密文本" {
		t.Errorf("Content = %q, want 加密文本", msg.Content)
	}
}

// --- 非文本消息 ---

func TestParseCallback_NonTextMessageKeepsEnvelope(t *testing.T) {
	// 图片消息没有 text 字段。消息本身仍然有效（后续可用 Meta 判断），
	// 但 Content 不应是原始 JSON 串——那会让下游把 {"image_key":...} 当用户输入。
	ev := richEvent{token: testToken, openID: "ou_1", chatID: "oc_1",
		messageID: "om_img", msgType: "image"}
	msg, err := newTestSource().ParseCallback(postRequest(ev.json()))
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if msg == nil {
		t.Fatal("ParseCallback = nil, want message (non-text still a valid message)")
	}
	if strings.Contains(msg.Content, "image_key") {
		t.Errorf("Content = %q, want empty for non-text (must not leak raw content JSON)", msg.Content)
	}
}

// --- body 还原 ---

func TestParseCallback_RestoresBody(t *testing.T) {
	original := richEvent{token: testToken, openID: "ou_1", chatID: "oc_1",
		messageID: "om_1", text: "hi"}.json()
	r := postRequest(original)

	if _, err := newTestSource().ParseCallback(r); err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}

	again, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("re-read body: %v", err)
	}
	if string(again) != original {
		t.Errorf("body = %q, want original (must be re-readable)", again)
	}
}

// --- 与 VerifyCallback 串联：完整入站链路 ---

func TestVerifyThenParse_FullInboundChain(t *testing.T) {
	// 这是 issue 的端到端链路：飞书 POST → 验签+解密 → 解析为 IncomingMessage
	ev := richEvent{
		token: testToken, openID: "ou_e2e", chatID: "oc_e2e", messageID: "om_e2e",
		text: "端到端", chatType: "group",
		mentions: []mentionSpec{{key: "@_user_1", openID: "ou_bot", name: "TaiJi"}},
	}
	r := postRequest(ev.json())
	s := newTestSource()

	if err := s.VerifyCallback(r); err != nil {
		t.Fatalf("VerifyCallback: %v", err)
	}
	msg, err := s.ParseCallback(r)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if msg == nil {
		t.Fatal("ParseCallback = nil")
	}
	if msg.UserID != "ou_e2e" || msg.ChatID != "oc_e2e" || msg.Content != "端到端" {
		t.Errorf("parsed = %+v, want open_id/chat_id/text to survive the chain", msg)
	}
}

// --- 契约断言（接口形状） ---

func TestSourceImplementsInboundSource(t *testing.T) {
	var _ channel.InboundSource = newTestSource()
}

func TestParseCallback_NilBodyFails(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/webhook/feishu", nil)
	r.Body = nil
	if _, err := newTestSource().ParseCallback(r); err == nil {
		t.Fatal("ParseCallback with nil body = nil, want error")
	}
}
