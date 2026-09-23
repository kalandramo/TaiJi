package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// 模型装配契约（issue #2 AC）：
//   1. 模型名为空 → 非 nil 错误（不得静默把空串传给 provider）
//   2. 不可达 base_url → 错误信息包含该 URL（不得泛化为 "connection failed"）

func TestNewModel_EmptyNameIsRejected(t *testing.T) {
	for _, name := range []string{"", "   ", "\t"} {
		m, err := NewModel(ModelConfig{Name: name, APIKey: "k"})
		if err == nil {
			t.Errorf("NewModel(name=%q) returned nil error, want error", name)
			continue
		}
		if m != nil {
			t.Errorf("NewModel(name=%q) returned non-nil model alongside error", name)
		}
		if !strings.Contains(err.Error(), "model") {
			t.Errorf("error %q should mention the missing model name", err.Error())
		}
	}
}

func TestNewModel_ValidConfigSucceeds(t *testing.T) {
	m, err := NewModel(ModelConfig{Name: "gpt-4o-mini", APIKey: "sk-test", BaseURL: "https://api.example.com/v1"})
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	if m == nil {
		t.Fatal("NewModel returned nil model with nil error")
	}
}

func TestNewModel_BaseURLReachesProvider(t *testing.T) {
	// 装配层不发起请求，只验证配置被正确携带。
	// 「错误信息含 URL」是端到端行为，由 TestModelError_ContainsBaseURL 断言。
	unreachable := "http://127.0.0.1:1/v1"
	m, err := NewModel(ModelConfig{Name: "gpt-4o-mini", APIKey: "k", BaseURL: unreachable})
	if err != nil {
		t.Fatalf("NewModel should not fail at assembly time: %v", err)
	}
	if m == nil {
		t.Fatal("model is nil")
	}
	if got := ResolveBaseURL(ModelConfig{Name: "gpt-4o-mini", APIKey: "k", BaseURL: unreachable}); got != unreachable {
		t.Errorf("ResolveBaseURL = %q, want %q", got, unreachable)
	}
}

func TestModelConfig_Defaults(t *testing.T) {
	// 未指定 base_url 时，解析结果应为空（让 provider 用官方默认），
	// 而不是我们硬编码一个错误的值。
	if got := ResolveBaseURL(ModelConfig{Name: "m", APIKey: "k"}); got != "" {
		t.Errorf("ResolveBaseURL with no BaseURL = %q, want empty", got)
	}
}

func TestModelConfig_EnvFallback(t *testing.T) {
	t.Setenv("TAIJI_MODEL_NAME", "env-model")
	t.Setenv("TAIJI_MODEL_API_KEY", "env-key")
	t.Setenv("TAIJI_MODEL_BASE_URL", "https://env.example.com/v1")

	cfg := ModelConfigFromEnv()
	if cfg.Name != "env-model" {
		t.Errorf("Name = %q, want env-model", cfg.Name)
	}
	if cfg.APIKey != "env-key" {
		t.Errorf("APIKey = %q, want env-key", cfg.APIKey)
	}
	if cfg.BaseURL != "https://env.example.com/v1" {
		t.Errorf("BaseURL = %q, want https://env.example.com/v1", cfg.BaseURL)
	}
}

// TestModelError_ContainsBaseURL 是 AC-4 的真实断言：经过 provider 发起
// 真实 HTTP 请求，验证返回的错误文本包含 base URL。
//
// 用 httptest 起一个返回 401 的端点（比不可达地址更稳：不依赖端口拒绝行为）。
// 这个测试之所以必要，是因为早期版本只断言了 WrapModelError 这个自造函数的
// 行为，而该函数并不在真实错误路径上（模型连接失败走事件流，不走同步返回）。
func TestModelError_ContainsBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	m, err := NewModel(ModelConfig{Name: "test-model", APIKey: "bad", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}

	// 直接驱动 provider，拿到它产生的错误。
	stream, err := m.GenerateContent(context.Background(), &model.Request{
		Messages: []model.Message{model.NewUserMessage("hi")},
	})
	if err != nil {
		// 同步返回的错误也必须含 URL
		if !strings.Contains(err.Error(), srv.URL) {
			t.Errorf("sync error %q must contain base URL %q", err.Error(), srv.URL)
		}
		return
	}

	var got string
	for resp := range stream {
		if resp != nil && resp.Error != nil {
			got = resp.Error.Message
		}
	}
	if got == "" {
		t.Fatal("no error surfaced from provider; cannot assert URL presence")
	}
	if !strings.Contains(got, srv.URL) {
		t.Errorf("provider error %q must contain base URL %q\n"+
			"（这是 AC-4 的真实契约：错误必须能让用户定位到端点）", got, srv.URL)
	}
}
