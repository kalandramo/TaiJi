// Package server 是端到端管道（issue #9）。
//
// 把各层串起来：门禁 → 路由 → 串行化 → 执行 → 出站。
// 依据设计文档 §4.7 的端到端数据流。
//
// 本包只做**装配**，不实现任何业务机制——机制都在各自包里：
//
//	门禁   internal/channel.EvaluateGate        （issue #6）
//	路由   internal/channel.ResolveRoute        （issue #7）
//	串行化 internal/concurrency.Serializer      （issue #8）
//	执行   internal/chat.Executor               （issue #2/#3/#4）
//	出站   internal/channel.Sender / feishu.Sender（issue #9 Wave 1）
//
// 这样分层的收益：管道本身是纯接线，可以用 fake 依赖完整测试，
// 而各层的语义由各自的测试覆盖。
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kalandramo/TaiJi/internal/authz"
	"github.com/kalandramo/TaiJi/internal/channel"
	"github.com/kalandramo/TaiJi/internal/concurrency"
)

// Executor 是单轮执行能力面。
//
// 在**消费方**（本包）定义接口，而非在 chat 包导出具体类型——
// 这样管道测试可以注入 fake，不必起真实的模型端点。
// chat.Executor 天然满足它（方法集一致）。
//
// sessionID 是**会话级**标识（管道传路由后的 effectiveJID）：
// 同一会话的消息共享历史（AC-3 后半），不同会话互相隔离。
// 传空值会被实现拒绝——漏传 sessionID 是装配缺陷，显式失败优于
// 让所有会话共用一份历史（静默串话）。
type Executor interface {
	// Execute 执行单轮，返回完整回答（无流式回调）。
	//
	// 保留此方法：它是不需要流式展示的调用方的入口，且既有实现与
	// 测试 fake 无需改动语义。
	Execute(ctx context.Context, sessionID, input string) (string, error)

	// ExecuteStream 执行单轮，逐块回调并返回完整回答（issue #10）。
	//
	// onChunk 非 nil 时每个内容块到达即回调——管道据此更新飞书卡片。
	// onChunk 为 nil 时行为与 Execute 一致。
	//
	// 回调**同步执行**：慢回调会拖慢生成。管道层负责节流（见
	// pipeline.go 的 chunkThrottle），不让每个 token 都触发网络请求。
	ExecuteStream(ctx context.Context, sessionID, input string, onChunk func(string)) (string, error)
}

// GateConfig 是门禁的静态配置部分（每次判定不变的量）。
//
// 逐条对应 channel.GateInput 的字段，但只保留静态项——
// ChatType/SenderID/Mentions 来自消息本身，不在这里。
type GateConfig struct {
	Activation channel.ActivationMode
	Audience   channel.AudienceMode
	BotOpenID  string
	Owners     []string
}

// Config 是管道的装配参数。
type Config struct {
	Sender   channel.Sender
	Executor Executor
	Gate     GateConfig
	Route    channel.RouteConfig

	// Logf 是日志出口。nil 则丢弃（管道不假设调用方有日志设施）。
	Logf func(format string, args ...any)

	// Placeholder 是占位消息文本。空则用默认值。
	//
	// 依据 §4.4.1：原型用「先发占位 → 收到回答后更新」的简化版流式，
	// 代替 happyclaw 的 CardKit 状态机。代价是看不到逐 token 打字效果。
	Placeholder string
}

// DefaultPlaceholder 是占位消息的默认文本。
const DefaultPlaceholder = "思考中…"

// Pipeline 是端到端管道。
type Pipeline struct {
	sender      channel.Sender
	executor    Executor
	gate        GateConfig
	route       channel.RouteConfig
	serializer  *concurrency.Serializer
	logf        func(format string, args ...any)
	placeholder string
}

// New 装配管道。
//
// 依赖缺失即报错，不构造一个「跑不起来」的管道——那会让配置错误
// 延迟到首条消息才暴露，而那时用户已在等回复。
func New(cfg Config) (*Pipeline, error) {
	if cfg.Sender == nil {
		return nil, errors.New("server: Sender is required")
	}
	if cfg.Executor == nil {
		return nil, errors.New("server: Executor is required")
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	placeholder := cfg.Placeholder
	if placeholder == "" {
		placeholder = DefaultPlaceholder
	}
	return &Pipeline{
		sender:      cfg.Sender,
		executor:    cfg.Executor,
		gate:        cfg.Gate,
		route:       cfg.Route,
		serializer:  concurrency.NewSerializer(),
		logf:        logf,
		placeholder: placeholder,
	}, nil
}

// Handle 处理一条入站消息。
//
// 返回 error 表示处理链中某处失败。注意：**返回 nil 不代表发出了回复**——
// 门禁拒绝是正常路径（AC-4 要求静默丢弃），不是错误。
func (p *Pipeline) Handle(ctx context.Context, msg *channel.IncomingMessage) error {
	if msg == nil {
		return errors.New("server: nil message")
	}

	// ── 1. 门禁 ──
	// 拒绝即返回：不发消息、不启动 run。这是 AC-4 的核心语义。
	decision := channel.EvaluateGate(channel.GateInput{
		Audience:   p.gate.Audience,
		Activation: p.gate.Activation,
		ChatType:   msg.ChatType,
		BotOpenID:  p.gate.BotOpenID,
		SenderID:   msg.UserID,
		Mentions:   msg.Mentions,
		Owners:     p.gate.Owners,
	})
	if !decision.Allow {
		// 静默丢弃：只记日志，不回复。用户不应感知到被拦。
		p.logf("server: gate rejected message_id=%s reason=%s", msg.MessageID, decision.Reason)
		return nil
	}

	// ── 2. 路由 ──
	// 会话标识含渠道前缀与 workspace，防跨渠道撞车（issue #7）。
	target := channel.ResolveRoute(p.route, msg)
	p.logf("server: routed message_id=%s jid=%s", msg.MessageID, target.EffectiveJID)

	// ── 3. 串行化 ──
	// 同 workspace 的消息共享串行化域（issue #8），保证同一份 session log
	// 不被并发写入。阻塞获取，ctx 取消可中断。
	release, err := p.serializer.AcquireBlocking(ctx, target.EffectiveJID)
	if err != nil {
		return fmt.Errorf("server: acquire serialization domain: %w", err)
	}
	defer release()

	// ── 4. 执行 ──
	// IM 来源降权为只读上下文（§4.3.3）：渠道只验签不验用户身份，
	// 无法可靠判定个人角色，故渠道来源的写操作一律拒绝。
	runCtx := authz.WithContextKind(ctx, authz.KindChannel)

	// 身份注入：把发送者身份放进 runCtx，供下游按用户判定。
	//
	// 为什么在这里：msg.UserID 来自平台元数据（飞书 sender.open_id，
	// 见 feishu/parse.go 的 extractOpenID），是**不可伪造**的身份源——
	// 不由调用方参数决定。门禁已用它做 owner 判定，此处让执行层也能读到。
	//
	// 框架会把该 ctx 透传到工具回调（beforeTool），故工具策略可据此
	// 按用户判定。实测验证见 internal/server/principal_e2e_test.go。
	//
	// 身份缺失时不注入（WithPrincipal 对空主体是 no-op）——下游
	// PrincipalFrom 返回 ok=false，调用方须据此 fail-closed。
	runCtx = authz.WithPrincipal(runCtx, authz.ResolvePrincipal(authz.PrincipalInput{
		ChannelID: p.route.WorkspaceID,
		Platform:  string(msg.Platform),
		OpenID:    msg.UserID,
	}))
	if _, ok := authz.PrincipalFrom(runCtx); !ok {
		// 身份缺失不阻断执行（向后兼容：CLI 等无渠道身份的场景），
		// 但必须留痕——否则"按用户管控"会在无声中失效。
		p.logf("server: principal missing message_id=%s platform=%s（下游无法按用户判定）",
			msg.MessageID, msg.Platform)
	}

	// ── 4. 执行 + 5. 出站 ──
	//
	// 两条路径（issue #10）：
	//   A. 渠道支持流式卡片 → 开卡片流，逐块更新（打字机效果 + Markdown）
	//   B. 否则（或不支持/失败）→ 既有「占位 → 更新」纯文本路径
	//
	// **卡片是增强，不是依赖**：任何一步失败都降级到 B，用户始终能得到回答。
	// 设计文档 §4.4 定义了降级矩阵。
	receiver, idType := receiverOf(msg)
	return p.deliverAnswer(runCtx, target.EffectiveJID, msg, receiver, idType)
}

// deliverAnswer 执行并把回答投递到渠道（含卡片流式与文本降级）。
//
// 抽成独立方法的原因：路径分支（卡片/文本）+ 降级逻辑让 handle 方法过长，
// 而这段逻辑可独立测试（给一个 fake StreamingSender 即可覆盖两条路径）。
func (p *Pipeline) deliverAnswer(
	runCtx context.Context,
	sessionID string,
	msg *channel.IncomingMessage,
	receiver string,
	idType channel.ReceiveIDType,
) error {
	// 尝试卡片流式路径。
	if ss, ok := p.sender.(channel.StreamingSender); ok {
		if err := p.streamViaCard(runCtx, ss, sessionID, msg, receiver, idType); err == nil {
			return nil // 卡片路径成功
		} else {
			// 降级：记日志后走文本路径。**不返回错误**——卡片失败不该
			// 让用户收不到回答（它是展示层增强）。
			p.logf("server: card streaming failed, falling back to text message_id=%s err=%v",
				msg.MessageID, err)
		}
	}
	return p.deliverAsText(runCtx, sessionID, msg, receiver, idType)
}

// streamViaCard 用卡片流式投递（issue #10）。
//
// 返回 error 表示卡片路径不可用（调用方据此降级）；返回 nil 表示成功。
func (p *Pipeline) streamViaCard(
	runCtx context.Context,
	ss channel.StreamingSender,
	sessionID string,
	msg *channel.IncomingMessage,
	receiver string,
	idType channel.ReceiveIDType,
) error {
	stream, err := ss.StartCardStream(runCtx, receiver, channel.SendOptions{
		ReceiveIDType: idType,
	})
	if err != nil {
		return fmt.Errorf("start card stream: %w", err)
	}

	// 节流器：避免每个 token 都触发网络请求（见 throttle.go）。
	th := newChunkThrottle()

	answer, execErr := p.executor.ExecuteStream(runCtx, sessionID, msg.Content,
		func(chunk string) {
			full := th.Add(chunk)
			if !th.ShouldFlush(time.Now()) {
				return
			}
			// 更新失败不中断生成——流结束时还有一次强制收尾。
			if err := stream.Update(runCtx, full); err != nil {
				p.logf("server: card update failed message_id=%s err=%v", msg.MessageID, err)
				return
			}
			th.MarkSent(full, time.Now())
		})

	// 收尾：无论成功与否都要 Close，否则卡片停留在"生成中"。
	final := answer
	if execErr != nil {
		final = "抱歉，处理时出错：" + execErr.Error()
		p.logf("server: execute failed message_id=%s err=%v", msg.MessageID, execErr)
	}
	if final == "" {
		final = th.Text() // 执行失败但已有部分内容时，保留已生成的
	}
	if err := stream.Close(runCtx, final); err != nil {
		return fmt.Errorf("close card stream: %w", err)
	}

	if execErr != nil {
		return fmt.Errorf("execute: %w", execErr)
	}
	return nil
}

// deliverAsText 是既有的「占位 → 更新」纯文本路径（issue #9）。
func (p *Pipeline) deliverAsText(
	runCtx context.Context,
	sessionID string,
	msg *channel.IncomingMessage,
	receiver string,
	idType channel.ReceiveIDType,
) error {
	// 先发占位消息，拿到 message_id 供后续更新（§4.4.1 的简化版流式）。
	placeholderID, err := p.sender.SendMessage(runCtx, receiver, p.placeholder, channel.SendOptions{
		ReceiveIDType: idType,
	})
	if err != nil {
		return fmt.Errorf("server: send placeholder: %w", err)
	}

	answer, execErr := p.executor.Execute(runCtx, sessionID, msg.Content)

	// ── 5. 出站 ──
	// 无论执行成功与否都要更新占位消息——否则用户会看到一条永远「思考中…」
	// 的空白回复。失败时更新为错误提示。
	final := answer
	if execErr != nil {
		final = "抱歉，处理时出错：" + execErr.Error()
		p.logf("server: execute failed message_id=%s err=%v", msg.MessageID, execErr)
	}
	if _, err := p.sender.SendMessage(runCtx, receiver, final, channel.SendOptions{
		ReceiveIDType: idType,
		MessageID:     placeholderID,
	}); err != nil {
		return fmt.Errorf("server: update message: %w", err)
	}

	if execErr != nil {
		return fmt.Errorf("server: execute: %w", execErr)
	}
	return nil
}

// receiverOf 决定出站的接收者与 ID 类型。
//
// 飞书私聊事件里 chat_id 为空（inbound.go:79），此时接收者只能用
// 发送者的 open_id。群聊则用 chat_id。让实现去猜会引入歧义——
// 同一字符串在不同命名空间下含义不同，猜错会投递到错误对象。
func receiverOf(msg *channel.IncomingMessage) (string, channel.ReceiveIDType) {
	if msg.ChatType == channel.ChatDirect || msg.ChatID == "" {
		return msg.UserID, channel.ReceiveIDOpen
	}
	return msg.ChatID, channel.ReceiveIDChat
}
