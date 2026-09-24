// Package channel 是渠道层。
//
// 依据设计文档 §4.4.1（docs/03-原型设计文档.md:437）拆为两层：
//
//   - 连接层 Channel：长驻连接的建立 / 出站 / 收尾
//   - 入站层（本文件）：平台事件 → 统一消息契约
//
// 分层根因：飞书有两种接入形态。webhook 是「被推送」——需要验签的 HTTP 入口；
// 长连接是「主动拉」——SDK 回调直出消息，没有 HTTP 入口，因而不需要验签。
//
// **webhook 形态已移除**（原型只用长连接）：原先的 InboundSource 接口
// （VerifyCallback / ParseCallback / HandleURLVerification）随之删除——
// 它只为 webhook 服务，长连接路径从不经过它。本文件现只保留两种形态
// 共用的消息契约。
package channel

// Platform 是渠道平台标识。
// 多渠道路由需要它来构造带前缀的会话标识（防跨渠道 chatID 撞车，FR-10.2）。
type Platform string

// PlatformFeishu 是飞书（Feishu/Lark）。
// 原型只做飞书，但接口按多渠道路由设计（docs/03-原型设计文档.md:42 §1.3）。
const PlatformFeishu Platform = "feishu"

// ChatType 是会话形态。
type ChatType string

const (
	// ChatDirect 是私聊。飞书平台值为 p2p，解析层负责归一化。
	ChatDirect ChatType = "direct"
	// ChatGroup 是群聊。群聊才有 @ 门禁概念（见 #6）。
	ChatGroup ChatType = "group"
)

// Mention 是被 @ 的对象。
//
// @ 判定必须用本元数据，不得用 strings.Contains(text, "@bot")——
// 用户手写 @name 会被误判为「bot 被提及」（docs/03-原型设计文档.md:584 §4.4.3）。
type Mention struct {
	// OpenID 是平台原生用户 ID（飞书为 id.open_id）。门禁比对的就是它。
	OpenID string
	// Key 是文本中的占位符（飞书为 @_user_1 形态），仅用于定位，不作身份判据。
	Key string
	// Name 是展示名，仅用于日志。
	Name string
}

// IncomingMessage 是统一消息。
//
// 字段对齐 happyclaw ChannelMessageMeta（happyclaw/src/types.ts:163-176），
// 见 docs/03-原型设计文档.md:798（§5 接口汇总）与 docs/02-设计文档.md:1035（§5.5.2）。
type IncomingMessage struct {
	Platform  Platform
	UserID    string // 平台原生 ID（飞书为 open_id），取自事件元数据
	ChatID    string // 群 ID（私聊为空）
	ChatType  ChatType
	MessageID string // 用于去重
	Content   string
	Mentions  []Mention
	Meta      *ChannelMessageMeta
}

// ChannelMessageMeta 是平台原生上下文元数据。
//
// 它是「路由与话题隔离」的输入（FR-10.3）：会话标识不能只看 ChatID，
// 否则同一群内不同话题会串话（docs/02-设计文档.md:1136 的话题虚拟 JID）。
//
// 关于 MentionedBot：docs/02-设计文档.md:1046 的字段集含该字段，但它的计算
// 依赖已配置的 bot_open_id，属触发门禁（#6）的输入面。本 Issue 不引入无法
// 真实填充的字段——#6 落地时再补，届时由门禁负责比对（§4.4.3 第 4 条）。
type ChannelMessageMeta struct {
	Provider          string // 渠道类型（飞书为 feishu）
	ChatType          string // 平台原值：p2p | group
	NativeContextType string // 仅 "thread" 视为话题（§4.4.4 的保守判定）
	ContextID         string
	ThreadID          string
	RootID            string
	MessageID         string
	Title             string
	Text              string
}
