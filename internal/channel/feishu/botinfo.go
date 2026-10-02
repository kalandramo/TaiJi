package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// bot /v3 /info 是「获取机器人自身信息」的端点，返回该应用对应 bot 的 open_id。
//
// 为什么用它而非让用户手填：open_id 是**应用维度的**、每个 bot 各不相同
// （多 agent 部署下有几个 bot 就有几个值）。手填既易错（复制粘贴串号），
// 又在换应用时要求同步改配置——而启动时本来就要用凭据换 token，
// 顺带取一次的成本近乎零。凭据已在手，open_id 是它的函数。
//
// 该端点未被 SDK 生成（源码注释明说 "not generated"，见
// larksuite/oapi-sdk-go v3.9.7 的 channel/channel.go:209），
// 故走 client.Get 的原生请求——与本仓库 send.go 用 lark.Client 而非
// 手写 HTTP 的取向一致（复用 token 缓存与刷新）。
const botInfoPath = "/open-apis/bot/v3/info"

// botInfoResponse 是 /open-apis/bot/v3/info 的响应体。
//
// 只声明本层要用的字段——平台该端点的字段多于此处，全量建模会随
// 平台演进腐化（与 event_fields.go 的取向一致）。
type botInfoResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Bot  struct {
		OpenID         string `json:"open_id"`
		AppName        string `json:"app_name"`
		ActivateStatus int    `json:"activate_status"`
	} `json:"bot"`
}

// FetchBotOpenID 用给定凭据取该应用对应 bot 的 open_id。
//
// 用途：启动期按 agent 各自的 app_id/app_secret 取 open_id，
// 构造门禁的 AppID → open_id 映射（每个 bot 的 open_id 不同）。
//
// 失败即返回 error 而非空串——空串会被门禁当作「未配置」而 fail-closed
// 拒绝该 bot 的所有群消息（表现为「bot 活着但永远不响应」），
// 静默降级比启动失败更难排查。调用方应据此中止启动。
func FetchBotOpenID(ctx context.Context, appID, appSecret string) (string, error) {
	return FetchBotOpenIDWithBaseURL(ctx, appID, appSecret, "")
}

// FetchBotOpenIDWithBaseURL 同 FetchBotOpenID，但可覆盖开放平台端点。
//
// openBaseURL 为空走 SDK 默认（生产路径）；非空指向假端点——
// 让测试打**真实 SDK 调用链**（token 换取、请求构造、响应解析），
// 而非测一个自造接口（与 send.go 的测试策略一致，见 send_test.go:18）。
func FetchBotOpenIDWithBaseURL(ctx context.Context, appID, appSecret, openBaseURL string) (string, error) {
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(appSecret) == "" {
		return "", fmt.Errorf(
			"feishu: fetching bot open_id requires app_id and app_secret " +
				"(configure via TAIJI_AGENTS)")
	}

	opts := []lark.ClientOptionFunc{}
	if u := strings.TrimSpace(openBaseURL); u != "" {
		opts = append(opts, lark.WithOpenBaseUrl(u))
	}
	client := lark.NewClient(appID, appSecret, opts...)

	resp, err := client.Get(ctx, botInfoPath, nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", fmt.Errorf("feishu: request bot info: %w", err)
	}

	var result botInfoResponse
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return "", fmt.Errorf("feishu: parse bot info: %w", err)
	}
	// SDK 的 *ApiResp 同时带 HTTP 层结果与业务 code。只判 err 不够——
	// 平台可能回 HTTP 200 而业务 code != 0（与 send.go 的判据一致）。
	if result.Code != 0 {
		return "", fmt.Errorf("feishu: bot info returned code=%d msg=%s",
			result.Code, result.Msg)
	}
	if result.Bot.OpenID == "" {
		return "", fmt.Errorf(
			"feishu: bot info returned empty open_id（应用 %s 的凭据可能无 bot 身份）", appID)
	}
	return result.Bot.OpenID, nil
}
