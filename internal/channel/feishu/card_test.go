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

// 卡片流式渲染测试（issue #10）。
//
// 用假端点断言**协议序列**——这是 AC-2/AC-4 的证据面：
//   - 发的是 interactive 消息（AC-1）
//   - 有多次内容更新（AC-2）
//   - sequence 递增、streaming_mode 正确开关（AC-4）

// cardCall 记录一次卡片相关调用。
type cardCall struct {
	Method string // HTTP 方法
	Path   string
	Body   string
}

// fakeCardkit 是卡片 API 的假端点，记录调用序列。
type fakeCardkit struct {
	mu    sync.Mutex
	calls []cardCall

	// 故障注入（测降级路径）
	failCreate  bool
	failContent bool
}

func (f *fakeCardkit) record(r *http.Request) string {
	var body string
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, cardCall{Method: r.Method, Path: r.URL.Path, Body: body})
	f.mu.Unlock()
	return body
}

func (f *fakeCardkit) snapshot() []cardCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cardCall(nil), f.calls...)
}

func writeCardJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeCardkit) handler() http.Handler {
	mux := http.NewServeMux()

	// token 获取（SDK 客户端初始化需要）
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		writeCardJSON(w, map[string]any{
			"code": 0, "msg": "ok",
			"tenant_access_token": "t-test", "expire": 7200,
		})
	})

	// 创建卡片实体：POST /open-apis/cardkit/v1/cards
	mux.HandleFunc("/open-apis/cardkit/v1/cards", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if f.failCreate {
			writeCardJSON(w, map[string]any{"code": 99991, "msg": "create failed"})
			return
		}
		writeCardJSON(w, map[string]any{
			"code": 0, "msg": "ok",
			"data": map[string]any{"card_id": "c_test_card"},
		})
	})

	// 卡片设置（streaming 开关）：PATCH /open-apis/cardkit/v1/cards/:id/settings
	mux.HandleFunc("/open-apis/cardkit/v1/cards/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if strings.HasSuffix(r.URL.Path, "/settings") {
			writeCardJSON(w, map[string]any{"code": 0, "msg": "ok"})
			return
		}
		// 元素内容更新：PUT .../elements/:eid/content
		if strings.Contains(r.URL.Path, "/elements/") && strings.HasSuffix(r.URL.Path, "/content") {
			if f.failContent {
				writeCardJSON(w, map[string]any{"code": 99992, "msg": "content failed"})
				return
			}
			writeCardJSON(w, map[string]any{"code": 0, "msg": "ok"})
			return
		}
		writeCardJSON(w, map[string]any{"code": 0, "msg": "ok"})
	})

	// 发消息：POST /open-apis/im/v1/messages
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeCardJSON(w, map[string]any{
			"code": 0, "msg": "ok",
			"data": map[string]any{"message_id": "om_card_msg"},
		})
	})

	return mux
}

func newCardSender(t *testing.T, f *fakeCardkit) (*Sender, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	// appID 唯一：SDK token 缓存是进程级全局，共用会串味（见 send_test.go 注释）
	s, err := NewSender(SenderConfig{
		AppID:       "cli_card_test_" + t.Name(),
		AppSecret:   "secret",
		OpenBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s, srv
}

// ===== AC-1：发的是 interactive 消息 =====

func TestCard_SendCardMessageIsInteractive(t *testing.T) {
	f := &fakeCardkit{}
	s, _ := newCardSender(t, f)

	if _, err := s.SendCardMessage(context.Background(), "ou_user", "c_abc",
		channel.SendOptions{ReceiveIDType: channel.ReceiveIDOpen}); err != nil {
		t.Fatalf("SendCardMessage: %v", err)
	}

	calls := f.snapshot()
	var msgBody string
	for _, c := range calls {
		if strings.Contains(c.Path, "/im/v1/messages") {
			msgBody = c.Body
		}
	}
	if msgBody == "" {
		t.Fatal("未发出消息")
	}
	if !strings.Contains(msgBody, `"interactive"`) {
		t.Errorf("msg_type 应为 interactive，实际 body = %s", msgBody)
	}
	// content 应引用 card_id
	if !strings.Contains(msgBody, "c_abc") {
		t.Errorf("content 应引用 card_id，实际 body = %s", msgBody)
	}
}

// ===== AC-4：流式协议序列 =====

func TestCard_StreamingProtocolSequence(t *testing.T) {
	f := &fakeCardkit{}
	s, _ := newCardSender(t, f)
	ctx := context.Background()

	stream, err := s.CreateCardStream(ctx, "")
	if err != nil {
		t.Fatalf("CreateCardStream: %v", err)
	}
	if stream.CardID() != "c_test_card" {
		t.Errorf("CardID = %q", stream.CardID())
	}

	if err := stream.SetStreaming(ctx, true); err != nil {
		t.Fatalf("SetStreaming(true): %v", err)
	}
	if err := stream.AppendContent(ctx, "第一块"); err != nil {
		t.Fatalf("AppendContent 1: %v", err)
	}
	if err := stream.AppendContent(ctx, "第一块第二块"); err != nil {
		t.Fatalf("AppendContent 2: %v", err)
	}
	if err := stream.SetStreaming(ctx, false); err != nil {
		t.Fatalf("SetStreaming(false): %v", err)
	}

	// 断言 sequence 严格递增
	var seqs []int
	for _, c := range f.snapshot() {
		if c.Body == "" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(c.Body), &body); err != nil {
			continue
		}
		if s, ok := body["sequence"].(float64); ok {
			seqs = append(seqs, int(s))
		}
	}
	if len(seqs) < 4 {
		t.Fatalf("应有 4 次带 sequence 的调用（settings×2 + content×2），实际 %d: %v", len(seqs), seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("sequence 必须严格递增，实际序列 = %v", seqs)
			break
		}
	}
	t.Logf("✓ sequence 序列 = %v（严格递增）", seqs)
}

// streaming_mode 的开关值必须正确。
func TestCard_StreamingModeToggles(t *testing.T) {
	f := &fakeCardkit{}
	s, _ := newCardSender(t, f)
	ctx := context.Background()

	stream, _ := s.CreateCardStream(ctx, "")
	_ = stream.SetStreaming(ctx, true)
	_ = stream.SetStreaming(ctx, false)

	var modes []bool
	for _, c := range f.snapshot() {
		if !strings.HasSuffix(c.Path, "/settings") {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(c.Body), &body); err != nil {
			continue
		}
		settings, _ := body["settings"].(string)
		if strings.Contains(settings, "true") {
			modes = append(modes, true)
		} else if strings.Contains(settings, "false") {
			modes = append(modes, false)
		}
	}
	if len(modes) != 2 || !modes[0] || modes[1] {
		t.Errorf("streaming_mode 开关序列 = %v, want [true false]", modes)
	}
}

// ===== 降级路径：创建失败应返回 error（调用方据此回退文本）=====

func TestCard_CreateFailureReturnsError(t *testing.T) {
	f := &fakeCardkit{failCreate: true}
	s, _ := newCardSender(t, f)

	if _, err := s.CreateCardStream(context.Background(), ""); err == nil {
		t.Fatal("创建失败应返回 error（调用方据此降级到文本）")
	}
}

func TestCard_ContentFailureReturnsError(t *testing.T) {
	f := &fakeCardkit{failContent: true}
	s, _ := newCardSender(t, f)
	ctx := context.Background()

	stream, err := s.CreateCardStream(ctx, "")
	if err != nil {
		t.Fatalf("CreateCardStream: %v", err)
	}
	if err := stream.AppendContent(ctx, "x"); err == nil {
		t.Fatal("内容更新失败应返回 error")
	}
}

// ===== 契约适配：channel.StreamingSender =====

// feishu.Sender 必须满足契约（编译期断言已在 card.go，此处验证运行时行为）。
func TestCard_ImplementsStreamingSender(t *testing.T) {
	f := &fakeCardkit{}
	s, _ := newCardSender(t, f)

	// 类型断言探测（管道就是这样用的）
	var sender channel.Sender = s
	ss, ok := sender.(channel.StreamingSender)
	if !ok {
		t.Fatal("feishu.Sender 应满足 channel.StreamingSender")
	}

	stream, err := ss.StartCardStream(context.Background(), "ou_user", "思考中…",
		channel.SendOptions{ReceiveIDType: channel.ReceiveIDOpen})
	if err != nil {
		t.Fatalf("StartCardStream: %v", err)
	}
	if stream == nil {
		t.Fatal("应返回非 nil 流")
	}
}

// StartCardStream 应完成三步：创建卡片 → 发卡片消息 → 开启 streaming。
func TestCard_StartCardStreamSequence(t *testing.T) {
	f := &fakeCardkit{}
	s, _ := newCardSender(t, f)

	stream, err := s.StartCardStream(context.Background(), "ou_user", "思考中…",
		channel.SendOptions{ReceiveIDType: channel.ReceiveIDOpen})
	if err != nil {
		t.Fatalf("StartCardStream: %v", err)
	}
	_ = stream

	var paths []string
	for _, c := range f.snapshot() {
		paths = append(paths, c.Method+" "+c.Path)
	}
	// 断言三步都发生了
	hasCreate, hasMsg, hasStreamingOn := false, false, false
	for _, p := range paths {
		if strings.Contains(p, "POST /open-apis/cardkit/v1/cards") && !strings.Contains(p, "/elements/") {
			hasCreate = true
		}
		if strings.Contains(p, "POST /open-apis/im/v1/messages") {
			hasMsg = true
		}
		if strings.Contains(p, "/settings") {
			hasStreamingOn = true
		}
	}
	if !hasCreate {
		t.Error("应创建卡片实体")
	}
	if !hasMsg {
		t.Error("应发送卡片消息")
	}
	if !hasStreamingOn {
		t.Error("应开启 streaming 模式")
	}
	t.Logf("✓ 调用序列: %v", paths)
}

// Close 应先写最终内容，再关闭 streaming。
func TestCard_CloseWritesFinalThenClosesStreaming(t *testing.T) {
	f := &fakeCardkit{}
	s, _ := newCardSender(t, f)

	stream, err := s.StartCardStream(context.Background(), "ou_user", "思考中…",
		channel.SendOptions{ReceiveIDType: channel.ReceiveIDOpen})
	if err != nil {
		t.Fatalf("StartCardStream: %v", err)
	}
	if err := stream.Close(context.Background(), "最终回答"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 找出 close 后的调用：最后一个 content 更新应在最后一个 settings 之前
	var contentIdx, settingsIdx []int
	for i, c := range f.snapshot() {
		if strings.HasSuffix(c.Path, "/content") {
			contentIdx = append(contentIdx, i)
		}
		if strings.HasSuffix(c.Path, "/settings") {
			settingsIdx = append(settingsIdx, i)
		}
	}
	if len(contentIdx) == 0 || len(settingsIdx) < 2 {
		t.Fatalf("调用不足：content=%v settings=%v", contentIdx, settingsIdx)
	}
	// 最后写入的 content 必须在最后一次 settings 之前（先写内容再关流式）
	lastContent := contentIdx[len(contentIdx)-1]
	lastSettings := settingsIdx[len(settingsIdx)-1]
	if lastContent > lastSettings {
		t.Errorf("最终内容应在关闭 streaming 之前写入（content@%d > settings@%d）", lastContent, lastSettings)
	}
	t.Logf("✓ content@%d 在 settings@%d 之前", lastContent, lastSettings)
}
