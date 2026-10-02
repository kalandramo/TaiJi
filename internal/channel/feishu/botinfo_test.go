package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 测试策略：真实 SDK + httptest 假飞书端点（与 send_test.go 同一策略）。
//
// 验证的是真实调用链——token 换取、原生 Get 请求构造、响应解析。
// 这些正是最容易出错处（端点拼写、AccessTokenType、字段路径）。

// fakeBotInfo 是只服务 /open-apis/bot/v3/info 的假端点。
type fakeBotInfo struct {
	// openID 是返回的 bot open_id。
	openID string
	// code 允许注入业务层失败（反证用）。
	code int
	// msg 是业务错误信息。
	msg string
	// rawBody 非空时直接返回它（模拟畸形响应）。
	rawBody string
	// status 非零时用它作 HTTP 状态码（模拟 HTTP 层失败）。
	status int

	// pathHit 记录收到的请求路径，供断言端点正确。
	pathHit string
}

func (f *fakeBotInfo) handler() http.Handler {
	mux := http.NewServeMux()

	// token 端点：SDK 先换 tenant_access_token。
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"code":                0,
			"msg":                 "ok",
			"tenant_access_token": "t-fake",
			"expire":              7200,
		})
	})

	mux.HandleFunc(botInfoPath, func(w http.ResponseWriter, r *http.Request) {
		f.pathHit = r.URL.Path
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		if f.rawBody != "" {
			_, _ = w.Write([]byte(f.rawBody))
			return
		}
		writeJSON(w, map[string]any{
			"code": f.code,
			"msg":  f.msg,
			"bot": map[string]any{
				"open_id":         f.openID,
				"app_name":        "测试机器人",
				"activate_status": 2,
			},
		})
	})
	return mux
}

func TestFetchBotOpenID_ReturnsOpenID(t *testing.T) {
	f := &fakeBotInfo{openID: "ou_bill_bot"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	openID, err := FetchBotOpenIDWithBaseURL(context.Background(),
		"cli_bill", "secret_bill", srv.URL)
	if err != nil {
		t.Fatalf("应成功取得 open_id，却报错: %v", err)
	}
	if openID != "ou_bill_bot" {
		t.Errorf("open_id = %q, want %q", openID, "ou_bill_bot")
	}
	if f.pathHit != botInfoPath {
		t.Errorf("请求路径 = %q, want %q", f.pathHit, botInfoPath)
	}
}

func TestFetchBotOpenID_NonZeroCodeIsError(t *testing.T) {
	// 业务层失败（HTTP 200 但 code != 0）必须报错——
	// 返回空串会让门禁静默 fail-closed，表现为「bot 永远不响应」。
	f := &fakeBotInfo{openID: "ou_x", code: 99991663, msg: "app not found"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	if _, err := FetchBotOpenIDWithBaseURL(context.Background(),
		"cli_x", "sx", srv.URL); err == nil {
		t.Fatal("业务 code != 0 应返回错误")
	}
}

func TestFetchBotOpenID_EmptyOpenIDIsError(t *testing.T) {
	// code=0 但 open_id 为空：畸形响应，必须报错而非返回空串。
	f := &fakeBotInfo{openID: ""}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	if _, err := FetchBotOpenIDWithBaseURL(context.Background(),
		"cli_x", "sx", srv.URL); err == nil {
		t.Fatal("open_id 为空应返回错误")
	}
}

func TestFetchBotOpenID_MalformedBodyIsError(t *testing.T) {
	f := &fakeBotInfo{rawBody: `not json`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	if _, err := FetchBotOpenIDWithBaseURL(context.Background(),
		"cli_x", "sx", srv.URL); err == nil {
		t.Fatal("响应体非 JSON 应返回错误")
	}
}

func TestFetchBotOpenID_MissingCredentialsIsError(t *testing.T) {
	// 凭据缺失：不发起请求即报错。
	if _, err := FetchBotOpenID(context.Background(), "", "s"); err == nil {
		t.Fatal("空 app_id 应返回错误")
	}
	if _, err := FetchBotOpenID(context.Background(), "cli", ""); err == nil {
		t.Fatal("空 app_secret 应返回错误")
	}
}

// 编译期断言：响应结构能解析 SDK 返回的实际 JSON 形态。
func TestBotInfoResponse_ParsesRealShape(t *testing.T) {
	// 这段 JSON 取自 SDK 自身的测试夹具
	// （oapi-sdk-go v3.9.7 channel/bot_identity_test.go:33），
	// 保证我们的结构声明与平台真实响应一致。
	raw := `{"code":0,"msg":"success","bot":{"open_id":"ou_bot_1","app_name":"Bot One","activate_status":2}}`
	var r botInfoResponse
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if r.Bot.OpenID != "ou_bot_1" {
		t.Errorf("open_id = %q, want %q", r.Bot.OpenID, "ou_bot_1")
	}
	if r.Bot.AppName != "Bot One" {
		t.Errorf("app_name = %q, want %q", r.Bot.AppName, "Bot One")
	}
}
