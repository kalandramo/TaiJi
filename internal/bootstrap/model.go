// Package bootstrap 负责把配置装配成可运行的组件。
//
// 本文件（issue #2）只做模型装配：从配置构建 trpc-agent-go 的 model.Model。
// 工具与渠道装配见后续 Issue。
package bootstrap

import (
	"fmt"
	"os"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
)

// 配置来源的环境变量名。控制值来自受信启动环境（设计文档 §4.6）。
const (
	EnvModelName    = "TAIJI_MODEL_NAME"
	EnvModelAPIKey  = "TAIJI_MODEL_API_KEY"
	EnvModelBaseURL = "TAIJI_MODEL_BASE_URL"
)

// ModelConfig 是模型装配所需的配置。
type ModelConfig struct {
	// Name 是模型标识（如 gpt-4o-mini / deepseek-chat）。必填。
	Name string
	// APIKey 是 provider 凭据。
	APIKey string
	// BaseURL 是 OpenAI 兼容端点。空表示用 provider 官方默认。
	BaseURL string
}

// ModelConfigFromEnv 从受信启动环境读取模型配置。
func ModelConfigFromEnv() ModelConfig {
	return ModelConfig{
		Name:    os.Getenv(EnvModelName),
		APIKey:  os.Getenv(EnvModelAPIKey),
		BaseURL: os.Getenv(EnvModelBaseURL),
	}
}

// ResolveBaseURL 返回生效的 base URL（空表示 provider 默认）。
// 单独抽出是为了让错误包装与测试都能拿到同一个值。
func ResolveBaseURL(cfg ModelConfig) string {
	return strings.TrimSpace(cfg.BaseURL)
}

// NewModel 构建 OpenAI 兼容 provider。
//
// 校验：模型名为空时返回错误——不得把空串静默传给 provider，
// 否则错误会延迟到首次请求且信息模糊（issue #2 AC）。
func NewModel(cfg ModelConfig) (model.Model, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return nil, fmt.Errorf(
			"bootstrap: model name is required (set %s or config field \"name\")",
			EnvModelName)
	}

	opts := make([]openai.Option, 0, 2)
	if k := strings.TrimSpace(cfg.APIKey); k != "" {
		opts = append(opts, openai.WithAPIKey(k))
	}
	if u := ResolveBaseURL(cfg); u != "" {
		opts = append(opts, openai.WithBaseURL(u))
	}

	return openai.New(name, opts...), nil
}
