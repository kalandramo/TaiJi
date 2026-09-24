package channel

import "context"

// 流式卡片能力契约（issue #10）。
//
// 为什么是**可选接口**而非扩展 Sender：
//   - 不是所有渠道都有卡片能力（纯文本渠道没有）
//   - 扩展 Sender 会迫使所有实现提供空方法——正是本包注释批评的形态
//   - 管道用类型断言探测能力，不支持时自动降级到纯文本
//
// 为什么定义在契约层而非编排层：`server` 生产代码不依赖具体渠道实现
// （实测确认：internal/server/*.go 无 feishu import）。若管道直接调
// feishu 的 CardKit，就会引入反向依赖，破坏该分层纪律。
//
// 与 SendMessage 的关系：SendMessage 管"发/更新一条消息"，
// StreamingSender 管"开一个可增量更新的卡片会话"。后者是前者的
// 富形态，但生命周期不同（会话有开始/更新/结束），故独立抽象。

// CardStream 是一次卡片流式会话。
//
// 生命周期：StartCardStream → Update（N 次）→ Close。
// **Close 必须被调用**——否则卡片停留在 streaming 状态，用户看到
// 永远"生成中"的卡片。
//
// 实现方（如 feishu.Sender）负责协议细节（sequence 递增、streaming_mode
// 开关），调用方只管"推送当前完整文本"。
type CardStream interface {
	// Update 推送当前**完整文本**（非增量）。
	//
	// 传完整文本而非增量：平台侧接口语义是"更新后的内容"（幂等），
	// 使重试安全——增量重试会重复拼接。
	Update(ctx context.Context, text string) error

	// Close 结束流式并写入最终内容。
	//
	// 即使前面 Update 失败也应调用（尽力收尾），避免卡片卡在 streaming 态。
	Close(ctx context.Context, finalText string) error
}

// StreamingSender 是支持卡片流式的出站实现。
//
// 用类型断言探测（`s.(StreamingSender)`），不支持的渠道返回 false，
// 管道据此降级到纯文本路径。
type StreamingSender interface {
	Sender

	// StartCardStream 创建卡片会话并发送卡片消息。
	//
	// placeholder 是卡片的**初始内容**，在创建卡片时就写入。
	//
	// 为什么必须传（实测缺陷）：首版传空串创建卡片，用户看到的是一个
	// **空白框**——从卡片消息发出到首个 chunk 到达（可能数秒，模型要
	// 先思考）之间，卡片没有任何内容。用户以为坏了。
	//
	// 不能在 StartCardStream 返回后再用 Update 补：那时卡片消息已经发出，
	// 用户仍会看到空窗。必须在**创建时**就有内容。
	//
	// 返回的 CardStream 供后续 Update/Close。若创建或发送失败，
	// 返回 error——调用方据此降级（**降级是调用方的职责**，
	// 契约层不替它决定，因为降级策略属编排）。
	StartCardStream(ctx context.Context, to, placeholder string, opts SendOptions) (CardStream, error)
}
