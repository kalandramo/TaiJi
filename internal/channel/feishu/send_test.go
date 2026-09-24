package feishu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 出站测试（issue #9 Wave 1）。
//
// 测试策略：**真实 SDK + httptest 假飞书端点**，而非自造接口 + fake 实现。
//
// 理由：SDK 提供了 WithOpenBaseUrl（client.go:176），可以把它指向本地假端点。
// 这样验证的是**真实调用链**——token 获取与缓存、请求构造（查询参数、body
// 序列化）、响应解析（code/data 字段路径）。若改测自己抽象的接口，则
// 「SDK 用对了没有」这件事完全没有被验证，而那正是最容易出错的地方
// （字段名、content 必须为 JSON 字符串、查询参数名）。

// fakeFeishu 是假飞书开放平台。
type fakeFeishu struct {
	mu sync.Mutex

	// requests 记录收到的请求，供断言。
	requests []recordedRequest

	// createStatus / updateStatus 允许注入失败响应（反证用）。
	createCode int
	updateCode int

	// tokenRequests 计数，用于验证 token 缓存生效（只取一次）。
	tokenRequests int
}

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func newFakeFeishu() *fakeFeishu {
	return &fakeFeishu{}
}

func (f *fakeFeishu) handler() http.Handler {
	mux := http.NewServeMux()

	// token 端点：SDK 用 tenant_access_token/internal 换 token。
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenRequests++
		f.mu.Unlock()
		writeJSON(w, map[string]any{
			"code":                0,
			"msg":                 "ok",
			"tenant_access_token": "t-fake-token",
			"expire":              7200,
		})
	})

	// 创建消息。
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		f.record(r, body)

		f.mu.Lock()
		code := f.createCode
		f.mu.Unlock()

		if code != 0 {
			writeJSON(w, map[string]any{"code": code, "msg": "create rejected"})
			return
		}
		writeJSON(w, map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]any{"message_id": "om_created"},
		})
	})

	// 更新消息：/open-apis/im/v1/messages/:message_id
	mux.HandleFunc("/open-apis/im/v1/messages/", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		f.record(r, body)

		f.mu.Lock()
		code := f.updateCode
		f.mu.Unlock()

		if code != 0 {
			writeJSON(w, map[string]any{"code": code, "msg": "update rejected"})
			return
		}
		writeJSON(w, map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]any{"message_id": "om_updated"},
		})
	})

	return mux
}

func (f *fakeFeishu) record(r *http.Request, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Body:   body,
	})
}

func (f *fakeFeishu) last() recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return recordedRequest{}
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeFeishu) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func readBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newTestSender 起假端点并构造指向它的 Sender。
func newTestSender(t *testing.T, f *fakeFeishu) *Sender {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	s, err := NewSender(SenderConfig{
		AppID:       "cli_fake",
		AppSecret:   "secret_fake",
		OpenBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s
}

// ===== 装配校验 =====

func TestNewSender_RequiresCredentials(t *testing.T) {
	// 凭据缺失即报错，不构造「发不出去」的实例——否则配置错误会延迟到
	// 首次发送才暴露，那时用户已在等回复。
	cases := []SenderConfig{
		{AppSecret: "s"},
		{AppID: "a"},
		{},
		{AppID: "  ", AppSecret: "s"},
	}
	for _, cfg := range cases {
		if _, err := NewSender(cfg); err == nil {
			t.Errorf("NewSender(%+v) = nil error, want error for missing credentials", cfg)
		}
	}
}

func TestSenderConfigFromEnv(t *testing.T) {
	got := SenderConfigFromEnv(map[string]string{
		EnvAppID:     "cli_x",
		EnvAppSecret: "sec_y",
	})
	if got.AppID != "cli_x" || got.AppSecret != "sec_y" {
		t.Errorf("SenderConfigFromEnv = %+v, want AppID=cli_x AppSecret=sec_y", got)
	}
}

// ===== 创建消息（AC-1 的出站半程） =====

func TestSender_CreateMessage(t *testing.T) {
	f := newFakeFeishu()
	s := newTestSender(t, f)

	id, err := s.SendMessage(context.Background(), "oc_group", "你好", channel.SendOptions{
		ReceiveIDType: channel.ReceiveIDChat,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != "om_created" {
		t.Errorf("message id = %q, want %q", id, "om_created")
	}

	req := f.last()
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.Path != "/open-apis/im/v1/messages" {
		t.Errorf("path = %s", req.Path)
	}
	// 查询参数必须带 receive_id_type——缺了平台无法解释 receive_id 的命名空间。
	if !strings.Contains(req.Query, "receive_id_type=chat_id") {
		t.Errorf("query = %q, want receive_id_type=chat_id", req.Query)
	}
	// body 关键字段：receive_id / msg_type=text / content 为 JSON 字符串。
	var body map[string]any
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		t.Fatalf("body is not JSON: %v (body=%s)", err, req.Body)
	}
	if body["receive_id"] != "oc_group" {
		t.Errorf("receive_id = %v, want oc_group", body["receive_id"])
	}
	if body["msg_type"] != "text" {
		t.Errorf("msg_type = %v, want text", body["msg_type"])
	}
	// content 必须是 **JSON 字符串**（飞书要求），不是裸文本、也不是 JSON 对象。
	content, ok := body["content"].(string)
	if !ok {
		t.Fatalf("content must be a JSON string, got %T (%v)", body["content"], body["content"])
	}
	var inner map[string]string
	if err := json.Unmarshal([]byte(content), &inner); err != nil {
		t.Fatalf("content is not valid JSON: %v (content=%s)", err, content)
	}
	if inner["text"] != "你好" {
		t.Errorf("content.text = %q, want 你好", inner["text"])
	}
}

func TestSender_CreateWithOpenIDForDirectChat(t *testing.T) {
	// 私聊：chat_id 为空（inbound.go:79），接收者只能是发送者 open_id。
	// 这条测试锁定「命名空间由调用方显式指定」的设计——不让实现去猜。
	f := newFakeFeishu()
	s := newTestSender(t, f)

	if _, err := s.SendMessage(context.Background(), "ou_user", "hi", channel.SendOptions{
		ReceiveIDType: channel.ReceiveIDOpen,
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	req := f.last()
	if !strings.Contains(req.Query, "receive_id_type=open_id") {
		t.Errorf("query = %q, want receive_id_type=open_id", req.Query)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(req.Body), &body)
	if body["receive_id"] != "ou_user" {
		t.Errorf("receive_id = %v, want ou_user", body["receive_id"])
	}
}

func TestSender_EmptyReceiverRejectedLocally(t *testing.T) {
	// 空接收者在本地拒绝，不浪费一次网络往返，也不让平台侧报出难懂的错。
	f := newFakeFeishu()
	s := newTestSender(t, f)

	if _, err := s.SendMessage(context.Background(), "", "x", channel.SendOptions{}); err == nil {
		t.Fatal("empty receiver must be rejected")
	}
	if n := len(f.all()); n != 0 {
		t.Errorf("no request should reach the platform, got %d", n)
	}
}

// ===== 更新消息（简化版流式的第二步） =====

func TestSender_UpdateMessage(t *testing.T) {
	f := newFakeFeishu()
	s := newTestSender(t, f)

	id, err := s.SendMessage(context.Background(), "oc_group", "完整回答", channel.SendOptions{
		MessageID: "om_placeholder",
	})
	if err != nil {
		t.Fatalf("SendMessage(update): %v", err)
	}
	if id != "om_updated" {
		t.Errorf("id = %q, want om_updated", id)
	}

	req := f.last()
	if req.Method != http.MethodPut {
		t.Errorf("method = %s, want PUT", req.Method)
	}
	if req.Path != "/open-apis/im/v1/messages/om_placeholder" {
		t.Errorf("path = %s, want /open-apis/im/v1/messages/om_placeholder", req.Path)
	}
	// 更新路径不应带 receive_id_type——接收者由 message_id 决定。
	if strings.Contains(req.Query, "receive_id_type") {
		t.Errorf("update query should not carry receive_id_type, got %q", req.Query)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(req.Body), &body)
	if body["msg_type"] != "text" {
		t.Errorf("msg_type = %v, want text", body["msg_type"])
	}
}

// ===== 错误传播（反证：失败不得被静默吞掉） =====

func TestSender_CreateBusinessErrorPropagates(t *testing.T) {
	// 平台可能回 HTTP 200 而业务 code != 0。只判 err 会漏掉这类失败，
	// 表现为「消息没发出去但调用方以为成功了」。
	f := newFakeFeishu()
	f.createCode = 230001
	s := newTestSender(t, f)

	_, err := s.SendMessage(context.Background(), "oc_group", "x", channel.SendOptions{})
	if err == nil {
		t.Fatal("business error (code != 0) must propagate, not be swallowed")
	}
	if !strings.Contains(err.Error(), "230001") {
		t.Errorf("error should carry the platform code, got %v", err)
	}
}

func TestSender_UpdateBusinessErrorPropagates(t *testing.T) {
	f := newFakeFeishu()
	f.updateCode = 230002
	s := newTestSender(t, f)

	_, err := s.SendMessage(context.Background(), "oc_group", "x", channel.SendOptions{
		MessageID: "om_1",
	})
	if err == nil {
		t.Fatal("update business error must propagate")
	}
	if !strings.Contains(err.Error(), "230002") {
		t.Errorf("error should carry the platform code, got %v", err)
	}
}

// ===== token 缓存（复用 SDK 而非手写的收益） =====

func TestSender_TokenFetchedOnceAndCached(t *testing.T) {
	// client.go:262 的 EnableTokenCache: true 让 SDK 缓存 token。
	// 若我们自己手写 HTTP，就得自己实现这套缓存与刷新——这条测试
	// 确认我们确实拿到了 SDK 的这个能力。
	//
	// **注意 appID 必须唯一**：SDK 的 token 缓存是进程级全局变量
	// （core/cache.go:21 `var cache = &localCache{}`），key 含 appID。
	// 若与其它测试共用 appID，缓存会命中，本测试的 token 计数就恒为 0
	// 而无法证明「缓存生效」——只会证明「全局缓存被污染了」。
	f := newFakeFeishu()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	s, err := NewSender(SenderConfig{
		AppID:       "cli_token_cache_isolated",
		AppSecret:   "secret",
		OpenBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.SendMessage(context.Background(), "oc_group", "x", channel.SendOptions{}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	f.mu.Lock()
	n := f.tokenRequests
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("token requests = %d, want 1 (SDK must cache the tenant_access_token)", n)
	}
}
