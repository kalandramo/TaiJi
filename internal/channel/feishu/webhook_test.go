package feishu

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// HTTP 边界测试：issue #5 的 Demo path 就是 curl 这个 handler。
// AC 原文：「token 不匹配的回调返回 403，且不产生任何 IncomingMessage」。

// recorder 收集 handler 的副作用，供断言「未产生 IncomingMessage」。
type recorder struct {
	mu       sync.Mutex
	messages []*channel.IncomingMessage
	logs     []string
	handler  *Handler
}

func newRecorder(cfg VerifyConfig) *recorder {
	r := &recorder{}
	r.handler = NewHandler(HandlerConfig{
		Verify: cfg,
		OnMessage: func(m *channel.IncomingMessage) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.messages = append(r.messages, m)
			return nil
		},
		Logf: func(format string, args ...any) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.logs = append(r.logs, fmt.Sprintf(format, args...))
		},
	})
	return r
}

func (r *recorder) messageCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

func (r *recorder) logText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

func (r *recorder) do(body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.handler.ServeHTTP(w, postRequest(body))
	return w
}

func (r *recorder) doRequest(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.handler.ServeHTTP(w, req)
	return w
}

// --- 安全边界：403 且不产生消息 ---

func TestHandler_ForgedTokenReturns403(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	w := r.do(feishuEventJSON("forged", "ou_1", "oc_1", "hi"))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	// AC: 且不产生任何 IncomingMessage
	if n := r.messageCount(); n != 0 {
		t.Errorf("produced %d IncomingMessage(s) on forged token, want 0", n)
	}
}

func TestHandler_MissingTokenConfigRejectsEverything(t *testing.T) {
	// AC: 未配置 verification token 时，端点拒绝所有回调（fail-closed），不是放行
	r := newRecorder(VerifyConfig{}) // 未配置 token

	cases := []string{
		feishuEventJSON(testToken, "ou_1", "oc_1", "hi"), // 连"看起来正确"的也拒
		feishuEventJSON("", "ou_1", "oc_1", "hi"),
		`{"challenge":"ch_1","type":"url_verification"}`,
		`{}`,
		``,
	}
	for i, body := range cases {
		w := r.do(body)
		if w.Code != http.StatusForbidden {
			t.Errorf("case %d: status = %d, want 403 (no token configured must fail closed)", i, w.Code)
		}
	}
	if n := r.messageCount(); n != 0 {
		t.Errorf("produced %d messages with no token configured, want 0", n)
	}
}

func TestHandler_DecryptFailureReturns403(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken, EncryptKey: testKey})
	plain := feishuEventJSON(testToken, "ou_1", "oc_1", "hi")
	body := `{"encrypt":"` + encryptFeishuBody(t, "wrong-key", plain) + `"}`

	if w := r.do(body); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (decrypt failure)", w.Code)
	}
	if n := r.messageCount(); n != 0 {
		t.Errorf("produced %d messages on decrypt failure, want 0", n)
	}
}

func TestHandler_MalformedJSONReturns403(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	if w := r.do(`{not json`); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (malformed body cannot pass verification)", w.Code)
	}
}

func TestHandler_EmptyBodyReturns403(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	if w := r.do(""); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestHandler_NonPOSTReturns405(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/webhook/feishu", nil)
		if w := r.doRequest(req); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, w.Code)
		}
	}
}

// --- 正常路径 ---

func TestHandler_CorrectTokenReturns200(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	w := r.do(feishuEventJSON(testToken, "ou_1", "oc_1", "hi"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if n := r.messageCount(); n != 1 {
		t.Fatalf("produced %d messages, want 1", n)
	}
}

func TestHandler_OnMessageReceivesParsedMessage(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	ev := richEvent{
		token: testToken, openID: "ou_sender", chatID: "oc_chat", messageID: "om_1",
		text: "群里的消息", chatType: "group",
		mentions: []mentionSpec{{key: "@_user_1", openID: "ou_bot", name: "TaiJi"}},
	}
	r.do(ev.json())

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(r.messages))
	}
	m := r.messages[0]
	if m.UserID != "ou_sender" {
		t.Errorf("UserID = %q, want ou_sender", m.UserID)
	}
	if m.ChatID != "oc_chat" {
		t.Errorf("ChatID = %q, want oc_chat", m.ChatID)
	}
	if m.Content != "群里的消息" {
		t.Errorf("Content = %q, want 群里的消息", m.Content)
	}
	if m.ChatType != channel.ChatGroup {
		t.Errorf("ChatType = %q, want group", m.ChatType)
	}
	if len(m.Mentions) != 1 || m.Mentions[0].OpenID != "ou_bot" {
		t.Errorf("Mentions = %+v, want one mention of ou_bot", m.Mentions)
	}
}

func TestHandler_EncryptedEventReturns200(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken, EncryptKey: testKey})
	ev := richEvent{token: testToken, openID: "ou_enc", chatID: "oc_enc", messageID: "om_1", text: "加密"}
	body := `{"encrypt":"` + encryptFeishuBody(t, testKey, ev.json()) + `"}`

	if w := r.do(body); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if n := r.messageCount(); n != 1 {
		t.Errorf("produced %d messages, want 1", n)
	}
}

// --- Demo path：日志打印解析后的 open_id / chat_id / 消息文本 ---

func TestHandler_LogsParsedFields(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	r.do(feishuEventJSON(testToken, "ou_logged", "oc_logged", "日志文本"))

	logs := r.logText()
	for _, want := range []string{"ou_logged", "oc_logged", "日志文本"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
}

func TestHandler_NeverLogsVerificationToken(t *testing.T) {
	// 日志会被采集、转发、归档。把凭据写进日志等于把凭据扩散到所有下游。
	r := newRecorder(VerifyConfig{VerificationToken: testToken, EncryptKey: testKey})

	// 覆盖成功、失败、挑战三条路径
	r.do(feishuEventJSON(testToken, "ou_1", "oc_1", "hi"))
	r.do(feishuEventJSON("forged", "ou_1", "oc_1", "hi"))
	r.do(`{"challenge":"ch_1","token":"` + testToken + `","type":"url_verification"}`)

	logs := r.logText()
	if strings.Contains(logs, testToken) {
		t.Errorf("logs contain the verification token:\n%s", logs)
	}
	if strings.Contains(logs, testKey) {
		t.Errorf("logs contain the encrypt key:\n%s", logs)
	}
}

func TestHandler_403BodyDoesNotLeakToken(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	w := r.do(feishuEventJSON("forged", "ou_1", "oc_1", "hi"))

	if strings.Contains(w.Body.String(), testToken) {
		t.Errorf("403 body leaks token: %q", w.Body.String())
	}
}

// --- 挑战请求 ---

func TestHandler_AnswersChallenge(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	body := `{"challenge":"ch_demo","token":"` + testToken + `","type":"url_verification"}`
	w := r.do(body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("challenge response is not JSON: %v (body=%q)", err, w.Body.String())
	}
	if got["challenge"] != "ch_demo" {
		t.Errorf("challenge = %q, want ch_demo", got["challenge"])
	}
	if n := r.messageCount(); n != 0 {
		t.Errorf("challenge request produced %d messages, want 0", n)
	}
}

// --- 非消息事件 ---

func TestHandler_NonMessageEventReturns200NoMessage(t *testing.T) {
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	ev := richEvent{token: testToken, eventType: "im.chat.disabled_v1", openID: "ou_1",
		chatID: "oc_1", messageID: "om_1", text: "hi"}
	w := r.do(ev.json())

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (valid signature, nothing to process)", w.Code)
	}
	if n := r.messageCount(); n != 0 {
		t.Errorf("produced %d messages for non-message event, want 0", n)
	}
}

// --- 处理器错误 ---

func TestHandler_OnMessageErrorReturns500(t *testing.T) {
	h := NewHandler(HandlerConfig{
		Verify: VerifyConfig{VerificationToken: testToken},
		OnMessage: func(*channel.IncomingMessage) error {
			return errors.New("downstream unavailable")
		},
		Logf: func(string, ...any) {},
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postRequest(feishuEventJSON(testToken, "ou_1", "oc_1", "hi")))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestHandler_NilOnMessageIsSafe(t *testing.T) {
	// 未接线处理器时（例如只做验签探针），handler 不应 panic。
	h := NewHandler(HandlerConfig{
		Verify: VerifyConfig{VerificationToken: testToken},
		Logf:   func(string, ...any) {},
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postRequest(feishuEventJSON(testToken, "ou_1", "oc_1", "hi")))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestHandler_DefaultLogfIsSafe(t *testing.T) {
	// 不传 Logf 时必须有可用默认值，不能 nil 调用 panic。
	h := NewHandler(HandlerConfig{Verify: VerifyConfig{VerificationToken: testToken}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postRequest(feishuEventJSON(testToken, "ou_1", "oc_1", "hi")))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --- 路径 ---

func TestHandler_DefaultPath(t *testing.T) {
	h := NewHandler(HandlerConfig{Verify: VerifyConfig{VerificationToken: testToken}})
	if h.Path() != "/webhook/feishu" {
		t.Errorf("Path() = %q, want /webhook/feishu", h.Path())
	}
}

func TestHandler_CustomPath(t *testing.T) {
	h := NewHandler(HandlerConfig{
		Verify: VerifyConfig{VerificationToken: testToken},
		Path:   "/hooks/lark",
	})
	if h.Path() != "/hooks/lark" {
		t.Errorf("Path() = %q, want /hooks/lark", h.Path())
	}
}
