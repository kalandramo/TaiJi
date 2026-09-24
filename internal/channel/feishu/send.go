package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 出站实现（issue #9）。
//
// 用 SDK 的 lark.Client 而非手写 HTTP：
//
//   - client.go:262 的 EnableTokenCache: true 让 SDK 自动获取并缓存
//     tenant_access_token（含过期刷新）。手写这套是重造轮子，且刷新竞态
//     容易写出难查的间歇性 401。
//   - 请求构造与响应解析由生成的 builder 承担，字段名有编译期保证。
//
// 长连接（LongConn）另行实现——它用 larkws，与出站正交。

// EnvAppID / EnvAppSecret 是出站与长连接所需的应用凭据键名。
//
// 与 internal/config 的 CredentialKeys 是同一组键的两个使用点：
// config 侧负责「工作区不得覆盖」，本包负责「从受信配置读取」。
// 一致性由 EnsureCredentialKeysProtected 在启动期断言。
const (
	EnvAppID     = "FEISHU_APP_ID"
	EnvAppSecret = "FEISHU_APP_SECRET"
)

// Sender 是飞书出站实现。
type Sender struct {
	client *lark.Client
}

// SenderConfig 是出站装配参数。
type SenderConfig struct {
	AppID     string
	AppSecret string

	// OpenBaseURL 覆盖开放平台端点。仅测试使用（httptest 假端点），
	// 生产留空走 SDK 默认 https://open.feishu.cn。
	//
	// 这个字段存在是因为 SDK 提供了 WithOpenBaseUrl（client.go:176）——
	// 它让测试可以打真实 SDK 调用链（token 获取 + 请求构造 + 响应解析），
	// 而不是测一个我自己抽象出来的假接口。
	OpenBaseURL string
}

// SenderConfigFromEnv 从受信配置快照构建出站配置。
func SenderConfigFromEnv(cfg map[string]string) SenderConfig {
	return SenderConfig{
		AppID:     cfg[EnvAppID],
		AppSecret: cfg[EnvAppSecret],
	}
}

// NewSender 构造出站实现。
//
// 凭据缺失即报错而非构造一个「发不出去」的实例：后者会让配置错误
// 延迟到首次发送才暴露，而那时用户已经在等回复了。
func NewSender(cfg SenderConfig) (*Sender, error) {
	if strings.TrimSpace(cfg.AppID) == "" || strings.TrimSpace(cfg.AppSecret) == "" {
		return nil, fmt.Errorf(
			"feishu: outbound requires %s and %s (set them in the startup environment)",
			EnvAppID, EnvAppSecret)
	}

	opts := []lark.ClientOptionFunc{}
	if u := strings.TrimSpace(cfg.OpenBaseURL); u != "" {
		opts = append(opts, lark.WithOpenBaseUrl(u))
	}
	return &Sender{client: lark.NewClient(cfg.AppID, cfg.AppSecret, opts...)}, nil
}

// SendMessage 实现 channel.Sender。
//
// 两步语义由 opts.MessageID 分流：
//   - 空 → 新建（POST /open-apis/im/v1/messages）
//   - 非空 → 更新（PUT /open-apis/im/v1/messages/:message_id）
//
// 文本 content 必须是 **JSON 字符串**（`{"text":"..."}`），不是裸文本——
// 飞书的 content 字段是「json 结构序列化后的字符串」（model.go:12390）。
// 直接把裸文本塞进去会被平台拒绝或显示为乱码。
func (s *Sender) SendMessage(ctx context.Context, to, text string, opts channel.SendOptions) (string, error) {
	if strings.TrimSpace(to) == "" {
		return "", fmt.Errorf("feishu: send requires a non-empty receiver id")
	}
	content, err := textContent(text)
	if err != nil {
		return "", err
	}

	if opts.MessageID != "" {
		return s.update(ctx, opts.MessageID, content)
	}
	return s.create(ctx, to, opts.ReceiveIDType, content)
}

// create 新建消息，返回新消息 ID。
func (s *Sender) create(ctx context.Context, to string, idType channel.ReceiveIDType, content string) (string, error) {
	body := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(to).
		MsgType(larkim.MsgTypeText).
		Content(content).
		Build()

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDTypeValue(idType)).
		Body(body).
		Build()

	resp, err := s.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: create message: %w", err)
	}
	// SDK 的 *Resp 同时携带 HTTP 层结果与业务 code（CodeError）。
	// 只判 err 不够——平台可能回 HTTP 200 而业务 code != 0。
	if !resp.Success() {
		return "", fmt.Errorf("feishu: create message rejected: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("feishu: create message returned no message_id")
	}
	return *resp.Data.MessageId, nil
}

// sendInteractive 新建一条 interactive（卡片）消息（issue #10）。
//
// 与 create 的唯一差异是 msg_type=interactive。抽成独立方法而非给 create
// 加参数：卡片消息没有"更新"语义（更新走 CardKit 的 Content，不在此），
// 故不复用 create 的 msgType 参数化路径。
func (s *Sender) sendInteractive(ctx context.Context, to string, idType channel.ReceiveIDType, content string) (string, error) {
	body := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(to).
		MsgType(larkim.MsgTypeInteractive).
		Content(content).
		Build()

	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDTypeValue(idType)).
		Body(body).
		Build()

	resp, err := s.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: create interactive message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: create interactive message rejected: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("feishu: create interactive message returned no message_id")
	}
	return *resp.Data.MessageId, nil
}

// update 更新已有消息内容。
func (s *Sender) update(ctx context.Context, messageID, content string) (string, error) {
	body := larkim.NewUpdateMessageReqBodyBuilder().
		MsgType(larkim.MsgTypeText).
		Content(content).
		Build()

	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()

	resp, err := s.client.Im.V1.Message.Update(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: update message: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: update message rejected: code=%d msg=%s", resp.Code, resp.Msg)
	}
	// Update 的响应也带 message_id；缺失时用入参 ID（更新语义下它就是答案）。
	if resp.Data != nil && resp.Data.MessageId != nil {
		return *resp.Data.MessageId, nil
	}
	return messageID, nil
}

// receiveIDTypeValue 把渠道层的类型映射为飞书 API 的查询参数值。
//
// 用字面量而非 SDK 常量：SDK v3.9.7 只导出 MsgType* 常量
// （ext_model.go:651-662），没有 ReceiveIdType* 常量集——查询参数值
// 由调用方自行提供。这里把字面量集中在一处，避免散落。
func receiveIDTypeValue(t channel.ReceiveIDType) string {
	if t == channel.ReceiveIDOpen {
		return "open_id"
	}
	return "chat_id"
}

// textContent 把文本包装成飞书要求的 content JSON 字符串。
func textContent(text string) (string, error) {
	raw, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", fmt.Errorf("feishu: encode text content: %w", err)
	}
	return string(raw), nil
}

// 编译期断言：Sender 满足渠道层出站契约。
var _ channel.Sender = (*Sender)(nil)
