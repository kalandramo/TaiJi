package feishu

import (
	"encoding/json"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 长连接事件的解析辅助。
//
// **历史**：本文件原名为 parse.go，同时承载 webhook 形态的事件解析
// （ParseCallback 及其配套结构 callbackEvent/eventHeader/eventBody/...）。
// webhook 形态已移除（只保留长连接），那些结构随之失去消费方，
// 于 2026-10-02 清理——留着会让读者以为还存在第二条入站路径。
//
// 现存的函数均为**长连接路径**所需（调用点集中在 longconn.go）。

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
