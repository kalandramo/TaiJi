package bootstrap

import (
	"strings"
	"testing"
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

func TestNewModel_BaseURLIsCarriedIntoError(t *testing.T) {
	// 用不可达地址装配本身不报错（错误发生在请求时），
	// 但装配后的 Info()/配置必须保留该 URL，供错误信息引用。
	unreachable := "http://127.0.0.1:1/v1"
	m, err := NewModel(ModelConfig{Name: "gpt-4o-mini", APIKey: "k", BaseURL: unreachable})
	if err != nil {
		t.Fatalf("NewModel should not fail at assembly time: %v", err)
	}
	got := ResolveBaseURL(ModelConfig{Name: "gpt-4o-mini", APIKey: "k", BaseURL: unreachable})
	if got != unreachable {
		t.Errorf("ResolveBaseURL = %q, want %q", got, unreachable)
	}
	if m == nil {
		t.Fatal("model is nil")
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

func TestWrapError_IncludesBaseURL(t *testing.T) {
	// AC: 不可达 base_url 的错误信息必须包含 URL。
	url := "http://127.0.0.1:1/v1"
	err := WrapModelError(url, errFake{msg: "connection failed"})
	if err == nil {
		t.Fatal("WrapModelError returned nil")
	}
	if !strings.Contains(err.Error(), url) {
		t.Errorf("error %q must contain the base URL %q", err.Error(), url)
	}
}

type errFake struct{ msg string }

func (e errFake) Error() string { return e.msg }
