package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 卡片流式渲染（issue #10）。
//
// 为什么需要它：飞书纯文本消息**不渲染 Markdown**——列表、代码块、粗体
// 都退化为裸字符（实测：Infraverse 查询返回的 `- 集群: xxx` 显示为连字符
// 文本）。卡片（msg_type=interactive）支持 Markdown 渲染。
//
// 流式协议（SDK 注释 model.go:2267 / :2615 原文为准）：
//
//	Settings(streaming_mode=true, sequence=N)
//	  → Content(sequence=N+1)   ← 可多次，sequence 必须递增
//	  → ...
//	  → Settings(streaming_mode=false, sequence=M)
//
// sequence **必须是递增正整数**，一次 streaming 周期内不递增会被平台拒绝。
// 用单调计数器而非时间戳——时钟回拨会导致 sequence 回退。
//
// 与 send.go 的分工：send.go 管 IM 消息（发/更新），本文件管卡片实体
// （创建/流式更新）。两者是两个 API 命名空间（im/v1 与 cardkit/v1）。

// CardElementID 是流式更新目标元素的 ID。
//
// 卡片 JSON 里该元素的 element_id 必须与此一致——平台按 ID 定位要更新的
// 元素。常量集中在一处，避免创建与更新两侧漂移。
const CardElementID = "answer"

// cardJSON 构造初始卡片 JSON。
//
// 结构（飞书卡片 schema 2.0）：
//
//	{
//	  "schema": "2.0",
//	  "config": {"streaming_mode": true},
//	  "body": {"elements": [{"tag":"markdown","element_id":"answer","content":""}]}
//	}
//
// 用 markdown 元素而非 plain_text——这是 Markdown 渲染的载体。
func cardJSON(initialText string) (string, error) {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"streaming_mode": true,
		},
		"body": map[string]any{
			"elements": []map[string]any{{
				"tag":        "markdown",
				"element_id": CardElementID,
				"content":    initialText,
			}},
		},
	}
	raw, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("feishu: encode card json: %w", err)
	}
	return string(raw), nil
}

// cardMessageContent 构造发送卡片消息所需的 content。
//
// IM 发卡片消息的 content 形态（与文本消息的 {"text":"..."} 不同）：
//
//	{"type":"card","data":{"card_id":"<card_id>"}}
func cardMessageContent(cardID string) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
	})
	if err != nil {
		return "", fmt.Errorf("feishu: encode card message content: %w", err)
	}
	return string(raw), nil
}

// streamingSettings 构造 Settings 请求体。
//
// streaming_mode 的开关经 settings JSON 传递（SDK 的 Settings 字段是
// 字符串，非结构化字段）——这是飞书 API 的形态，不是本层的选择。
func streamingSettings(enabled bool) (string, error) {
	raw, err := json.Marshal(map[string]any{"streaming_mode": enabled})
	if err != nil {
		return "", fmt.Errorf("feishu: encode streaming settings: %w", err)
	}
	return string(raw), nil
}

// CardStream 是一次卡片流式会话。
//
// 生命周期：Create → SendMessage(卡片) → AppendN 次 → Close。
// 所有方法在失败时返回 error，调用方据此走降级路径（见设计文档 §4.4）。
type CardStream struct {
	client *lark.Client
	cardID string

	// seq 是单调递增的 sequence。用 atomic 因为流式更新可能来自
	// 不同 goroutine（当前是同步的，但 atomic 让契约更明确）。
	seq atomic.Int64
}

// nextSeq 返回下一个 sequence 值。
//
// 从 1 开始（SDK 要求正整数），每次调用递增。
func (c *CardStream) nextSeq() int {
	return int(c.seq.Add(1))
}

// CardID 返回卡片实体 ID（供发送消息时引用）。
func (c *CardStream) CardID() string { return c.cardID }

// CreateCardStream 创建卡片实体。
//
// 返回的 CardStream 需由调用方负责收尾（Close）——否则卡片会停留在
// streaming 状态，用户看到永远在"生成中"的卡片。
func (s *Sender) CreateCardStream(ctx context.Context, initialText string) (*CardStream, error) {
	cardData, err := cardJSON(initialText)
	if err != nil {
		return nil, err
	}

	body := larkcardkit.NewCreateCardReqBodyBuilder().
		Type("card_json").
		Data(cardData).
		Build()

	resp, err := s.client.Cardkit.V1.Card.Create(ctx,
		larkcardkit.NewCreateCardReqBuilder().Body(body).Build())
	if err != nil {
		return nil, fmt.Errorf("feishu: create card: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("feishu: create card rejected: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.CardId == nil {
		return nil, fmt.Errorf("feishu: create card returned no card_id")
	}
	return &CardStream{client: s.client, cardID: *resp.Data.CardId}, nil
}

// SendCardMessage 发送一条卡片消息（引用已创建的卡片实体）。
//
// 与 SendMessage 的区别：msg_type=interactive，content 引用 card_id
// 而非内联文本。返回 message_id（供后续需要时定位消息）。
func (s *Sender) SendCardMessage(ctx context.Context, to, cardID string, opts channel.SendOptions) (string, error) {
	if strings.TrimSpace(to) == "" {
		return "", fmt.Errorf("feishu: send card requires a non-empty receiver id")
	}
	content, err := cardMessageContent(cardID)
	if err != nil {
		return "", err
	}
	return s.sendInteractive(ctx, to, opts.ReceiveIDType, content)
}

// SetStreaming 开关卡片的流式模式。
//
// 开启后卡片进入 streaming 状态，可接受 Content 增量更新；
// 关闭后卡片定稿。**必须成对调用**——只开不关会让卡片永远显示生成中。
func (c *CardStream) SetStreaming(ctx context.Context, enabled bool) error {
	settings, err := streamingSettings(enabled)
	if err != nil {
		return err
	}
	body := larkcardkit.NewSettingsCardReqBodyBuilder().
		Settings(settings).
		Sequence(c.nextSeq()).
		Build()

	resp, err := c.client.Cardkit.V1.Card.Settings(ctx,
		larkcardkit.NewSettingsCardReqBuilder().
			CardId(c.cardID).
			Body(body).
			Build())
	if err != nil {
		return fmt.Errorf("feishu: set streaming=%v: %w", enabled, err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: set streaming=%v rejected: code=%d msg=%s", enabled, resp.Code, resp.Msg)
	}
	return nil
}

// AppendContent 把**完整文本**写入卡片元素。
//
// 传完整文本而非增量：SDK 字段注释写 "更新后的文本内容"（model.go:2613）——
// 传完整内容是幂等的，重试语义更简单（增量重试会重复拼接）。
func (c *CardStream) AppendContent(ctx context.Context, text string) error {
	body := larkcardkit.NewContentCardElementReqBodyBuilder().
		Content(text).
		Sequence(c.nextSeq()).
		Build()

	resp, err := c.client.Cardkit.V1.CardElement.Content(ctx,
		larkcardkit.NewContentCardElementReqBuilder().
			CardId(c.cardID).
			ElementId(CardElementID).
			Body(body).
			Build())
	if err != nil {
		return fmt.Errorf("feishu: append card content: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: append card content rejected: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// ── 实现 channel.StreamingSender 契约（issue #10）──
//
// 适配层：把契约的 StartCardStream/Update/Close 映射到 CardKit 的
// CreateCardStream/SetStreaming/AppendContent。契约层不暴露 CardKit 概念
// （card_id、sequence、element_id），调用方只管推送完整文本。

// cardStreamAdapter 把 CardStream 适配为 channel.CardStream。
type cardStreamAdapter struct {
	inner *CardStream
}

// Update 推送当前完整文本。
func (a *cardStreamAdapter) Update(ctx context.Context, text string) error {
	return a.inner.AppendContent(ctx, text)
}

// Close 结束流式并写入最终内容。
//
// 顺序：先写最终内容再关 streaming_mode——反过来的话，关闭后平台可能
// 忽略后续内容更新，最终态就丢了。
//
// 即使写入失败也尝试关闭 streaming_mode：卡片卡在"生成中"比内容不完整
// 更糟（用户会一直等）。关闭失败才返回错误。
func (a *cardStreamAdapter) Close(ctx context.Context, finalText string) error {
	writeErr := a.inner.AppendContent(ctx, finalText)
	closeErr := a.inner.SetStreaming(ctx, false)
	if closeErr != nil {
		return closeErr // 关闭失败更严重（卡片卡住）
	}
	return writeErr
}

// StartCardStream 实现 channel.StreamingSender。
//
// 两步：创建卡片实体 → 发送卡片消息。任一步失败都返回 error，
// 调用方据此降级到纯文本（见设计文档 §4.4）。
func (s *Sender) StartCardStream(ctx context.Context, to string, opts channel.SendOptions) (channel.CardStream, error) {
	stream, err := s.CreateCardStream(ctx, "")
	if err != nil {
		return nil, err
	}
	if _, err := s.SendCardMessage(ctx, to, stream.CardID(), opts); err != nil {
		return nil, err
	}
	// 开启流式模式——之后才能接受 Content 更新。
	if err := stream.SetStreaming(ctx, true); err != nil {
		return nil, err
	}
	return &cardStreamAdapter{inner: stream}, nil
}

// 编译期断言：feishu.Sender 满足流式契约。
var _ channel.StreamingSender = (*Sender)(nil)
