package channel

import "context"

// 出站接口（issue #9）。
//
// 为什么只有出站一个方法：设计文档 §4.4.1 定义了统一的 Channel 接口
// （Platform/Connect/Disconnect/SendMessage/IsConnected），但该节自己论证过
// 「单接口必然让长连接模式实现三个永不调用的方法」。反方向同样成立——
// webhook 模式下 Disconnect()/IsConnected() 是空方法。本包不引入空方法，
// 只定义真正被消费的能力面。
//
// 长连接的生命周期（Start/Stop）不在此接口内：它是 feishu 包的具体类型
// （LongConn），只有长连接模式才需要，且形状与出站正交。

// ReceiveIDType 是出站接收者 ID 的类型。
//
// 为什么必须显式传类型：飞书私聊事件里 chat_id 为空（见 inbound.go:79
// 「群 ID（私聊为空）」），此时接收者只能用发送者的 open_id。
// 让实现去猜「这个 ID 是群还是人」会引入歧义——同一个字符串在不同
// 命名空间下含义不同，猜错会投递到错误对象。
type ReceiveIDType string

const (
	// ReceiveIDChat 是群会话 ID（chat_id），用于群消息。
	ReceiveIDChat ReceiveIDType = "chat_id"
	// ReceiveIDOpen 是用户 open_id，用于私聊。
	ReceiveIDOpen ReceiveIDType = "open_id"
)

// MessageKind 是出站消息的形态（issue #10）。
//
// 引入理由：卡片与文本的 content 结构不同（卡片是 {"type":"card","data":...}，
// 文本是 {"text":"..."}），而 msg_type 也不同（interactive vs text）。
// 形态是**渠道无关概念**（其他渠道也可能有富文本形态），故定义在契约层。
type MessageKind string

const (
	// KindText 是纯文本消息（既有行为）。
	// 注意：飞书纯文本**不渲染 Markdown**——列表、代码块、粗体都退化为裸字符。
	KindText MessageKind = "text"
	// KindCard 是交互式卡片（Markdown 渲染 + 可流式更新）。
	KindCard MessageKind = "card"
)

// SendOptions 控制一次出站。
type SendOptions struct {
	// ReceiveIDType 指定 To 的命名空间。空则默认为 ReceiveIDChat。
	ReceiveIDType ReceiveIDType

	// MessageID 非空 = 更新该消息（简化版流式：先发占位，再替换为完整回答）。
	// 空 = 新建消息，返回新消息的 ID。
	//
	// 对齐设计文档 §4.4.1：原型刻意省掉 happyclaw 的 StreamSender/CardKit
	// 状态机，用「占位消息 → 更新消息」替代。代价是看不到逐 token 打字效果，
	// 收益是省掉整个卡片状态机。
	//
	// 注：issue #10 已补上 CardKit 流式路径（Kind=KindCard 时走该路径）。
	MessageID string

	// Kind 指定消息形态。空 = KindText（向后兼容，既有调用无需改动）。
	//
	// 未知值经 EffectiveKind 回退为 KindText——fail-safe：宁可不渲染卡片，
	// 也不发一条平台不认的消息。
	Kind MessageKind
}

// EffectiveKind 返回生效的消息形态。
//
// 空值与未知值都回退 KindText：前者保证向后兼容，后者保证 fail-safe。
// 把这条规则集中在一处，避免各实现各自判断而漂移。
func (o SendOptions) EffectiveKind() MessageKind {
	switch o.Kind {
	case KindCard:
		return KindCard
	default:
		return KindText
	}
}

// Sender 是出站能力面。渠道实现（feishu.Sender）负责协议细节。
//
// 抽成接口的理由是**测试接缝**：管道层（internal/server）需要在不联网的
// 前提下断言「哪些消息被发了、发了什么」，以及 AC-4 的「零出站调用」。
type Sender interface {
	// SendMessage 发送或更新一条文本消息。
	//
	// to 的语义由 opts.ReceiveIDType 决定（群 chat_id 或用户 open_id）。
	// 返回消息 ID：新建时是新消息的 ID，更新时是被更新的 ID。
	// 调用方用它做后续的占位→替换两步流式。
	SendMessage(ctx context.Context, to, text string, opts SendOptions) (string, error)
}
