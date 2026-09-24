package feishu

import (
	"encoding/json"
	"errors"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// eventTypeMessage 是飞书「收到消息」事件类型。其余事件一律返回 nil。
const eventTypeMessage = "im.message.receive_v1"

// callbackEvent 是飞书 2.0 事件体的解析结构。
//
// 只声明本层要用的字段——飞书事件体字段极多，全量建模会让结构随平台演进
// 持续腐化。未声明字段被 encoding/json 忽略，不影响解析。
//
// 指针用于区分「字段缺失」与「字段为空串」：两者对安全判定意义不同
// （见 ParseCallback 对 sender 的处理）。
type callbackEvent struct {
	Header *eventHeader `json:"header"`
	Event  *eventBody   `json:"event"`
}

type eventHeader struct {
	EventID   string `json:"event_id"`
	Token     string `json:"token"`
	EventType string `json:"event_type"`
}

type eventBody struct {
	Sender  *eventSender  `json:"sender"`
	Message *eventMessage `json:"message"`
}

type eventSender struct {
	SenderID *senderID `json:"sender_id"`
}

type senderID struct {
	// OpenID 是主体的规范身份来源。
	OpenID string `json:"open_id"`
	// UserID / UnionID 仅用于日志对照，**不得**作为主体 ID——
	// 它们是租户内/跨应用 ID，与渠道身份空间不同源。
	UserID  string `json:"user_id"`
	UnionID string `json:"union_id"`
}

type eventMessage struct {
	MessageID string         `json:"message_id"`
	ChatID    string         `json:"chat_id"`
	ChatType  string         `json:"chat_type"`
	Content   string         `json:"content"`
	ThreadID  string         `json:"thread_id"`
	RootID    string         `json:"root_id"`
	Mentions  []eventMention `json:"mentions"`
}

type eventMention struct {
	Key  string     `json:"key"`
	Name string     `json:"name"`
	ID   *mentionID `json:"id"`
}

type mentionID struct {
	OpenID string `json:"open_id"`
}

// extractOpenID 从 sender 元数据取主体 ID。
//
// 缺失即报错而非降级为空串：空 UserID 会让下游 owner 比对恒不成立
// （§4.4.5 的 fail-closed 要求——无 owner 信息时拒绝，不放行），
// 且错误在解析点暴露比在门禁处暴露更容易定位。
func extractOpenID(sender *eventSender) (string, error) {
	if sender == nil || sender.SenderID == nil || sender.SenderID.OpenID == "" {
		return "", errors.New("feishu: message event has no sender open_id")
	}
	return sender.SenderID.OpenID, nil
}

// normalizeChatType 把平台值归一化为渠道层的 ChatType。
//
// **未知值归为 group 是有意的 fail-closed 方向**：群聊要过 @ 门禁，
// 私聊直接放行。把未知值当 direct 会让门禁被绕过——这正是
// docs/03-原型设计文档.md:576 记录的那类静默失效。
func normalizeChatType(platformValue string) channel.ChatType {
	if platformValue == "p2p" {
		return channel.ChatDirect
	}
	return channel.ChatGroup
}

// nativeContextType 做话题的保守判定：仅 thread_id 存在才视为话题。
//
// 依据：docs/03-原型设计文档.md:1157（§5.5.3「飞书的保守判定」）——
// 仅当显式 thread 才视为话题，避免把普通消息误判为独立会话而串话。
func nativeContextType(threadID string) string {
	if threadID != "" {
		return "thread"
	}
	return ""
}

// extractMentions 从元数据取 @ 列表。
//
// 必须用元数据而非文本匹配：用户手写 "@TaiJi" 会被 strings.Contains 误判为
// 「bot 被提及」。门禁比对的是 id.open_id（docs/03-原型设计文档.md:584）。
//
// 丢弃 open_id 为空的条目：这种条目无法参与身份比对，
// 留着只会让门禁的 containsMention 出现「看似命中实则无身份」的假阳性。
func extractMentions(in []eventMention) []channel.Mention {
	if len(in) == 0 {
		return nil
	}

	out := make([]channel.Mention, 0, len(in))
	for _, m := range in {
		if m.ID == nil || m.ID.OpenID == "" {
			continue
		}
		out = append(out, channel.Mention{
			OpenID: m.ID.OpenID,
			Key:    m.Key,
			Name:   m.Name,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractText 从 message.content 取出文本。
//
// 飞书的 content 是一个 **JSON 字符串**（如 `{"text":"hello"}`），不是纯文本。
// 直接把 content 当 Content 会让下游拿到 `{"text":"hello"}` 这种带转义的
// 原始串，把结构化噪声当用户输入。
//
// 判定方式是「能否解析出 text 字段」，而不是「message_type 是否等于 text」：
// 后者在 message_type 缺失或新增文本类消息类型时会把正文丢成空串——
// 而前者的行为对非文本消息（image 的 `{"image_key":...}`、
// post 的 `{"title":...,"content":[[...]]}`）同样是返回空串，两者结果一致，
// 但前者不会因为信封字段缺失而丢失用户输入。
//
// **代价（明示）**：富文本（post）的正文在 content 数组里，text 字段不存在，
// 因此其 Content 为空。若后续需要富文本，应在此函数补 post 分支——
// 它不影响其他字段的正确性。
//
// 非文本消息的「类型不支持」由 unsupportedKind 单独判定并标记到
// IncomingMessage.UnsupportedKind（见该函数）——本函数只负责取文本。
func extractText(content string) string {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	return payload.Text
}

// textMessageTypes 是「正文能由 extractText 取出」的消息类型。
//
// 依据飞书消息类型清单（open.feishu.cn 的 message_content 文档）：
// text 是纯文本；post 是富文本（正文在 content 数组，本原型不解析）。
// 其余类型（image/file/audio/media/sticker 等）的 content 里没有 text 字段。
//
// 为何把 post 也列为「文本类」：它是**用户能输入正文**的消息类型，
// 报「不支持图片」会误导。它的 Content 为空是解析能力不足，
// 属于另一类问题（见 extractText 的代价说明）。
var textMessageTypes = map[string]bool{
	"text": true,
	"post": true,
}

// unsupportedKind 判定「消息类型本身不被支持」，返回平台原生类型名。
//
// 触发条件（三者同时满足，见 channel.IncomingMessage.UnsupportedKind 的注释）：
//   - 正文为空（extractText 没取到 text）
//   - 平台声明了非空 message_type
//   - 该类型不在 textMessageTypes 里
//
// 为何要求「正文为空」：用户发文本消息但正文是空白时，message_type=text，
// 不会被标记（text 在 textMessageTypes 里）。若用户发了 image 但（不可能地）
// content 里带 text 字段，也不标记——有正文就优先按正文处理。
//
// 为何要求「类型非空」：类型缺失时不标记。缺字段是信封残缺，
// 把它报成「类型不支持」会把解析问题伪装成能力问题。
func unsupportedKind(messageType, text string) string {
	if text != "" || messageType == "" {
		return ""
	}
	if textMessageTypes[messageType] {
		return ""
	}
	return messageType
}
