package main

import (
	"fmt"
	"os"
	"strings"
)

// envAgentsKey 是多 agent 配置的环境变量名。
//
// 单个键、值内含全部 agent（分号分隔条目）——与既有 TAIJI_RBAC 同形态
// （见 envRBAC）。额外收益：凭据保护面是**单个键**，无需前缀匹配
// （对比 TAIJI_AGENT_<name>_APP_ID 的形态需加 ReservedPrefixes）。
const envAgentsKey = "TAIJI_AGENTS"

// defaultAgentName 是单 agent（未配 TAIJI_AGENTS）时的 agent 名。
//
// 与 TaiJi/internal/chat/execute.go 里 llmagent.New 的硬编码名一致，
// 保证既有部署升级后 agent 名不变。
const defaultAgentName = "assistant"

// agentSpec 是一个 agent 的配置（名 + 其绑定的飞书应用凭据）。
type agentSpec struct {
	// Name 是 agent 标识，用于日志与执行层选择。
	Name string
	// AppID / AppSecret 是该 agent 绑定的飞书应用凭据（ak/sk）。
	//
	// 多 bot 部署下每个 agent 一套——入站按事件头的 Header.AppID 分流，
	// 出站按同一 agent 选凭据（形成闭环）。
	AppID     string
	AppSecret string
}

// parseAgents 解析 TAIJI_AGENTS（多 agent 配置）。
//
// 格式：分号分隔 agent，逗号分隔字段（name / app_id / app_secret）：
//
//	TAIJI_AGENTS="name=billing,app_id=cli_aaa,app_secret=s1;name=ops,app_id=cli_bbb,app_secret=s2"
//
// 未配置时**回退到单 agent**（读旧的 FEISHU_APP_ID / FEISHU_APP_SECRET）——
// 保证既有部署零改动（向后兼容）。这是本函数最重要的不变量。
//
// 配置有误即返回 error（fail-fast）——错误信息**指出是哪个 agent**，
// 否则多 agent 下用户不知道哪条写错了。与 envRBAC 的取向一致：
// 「配了却不生效」类静默失效最难排查。
func parseAgents() ([]agentSpec, error) {
	raw := strings.TrimSpace(os.Getenv(envAgentsKey))
	if raw == "" {
		return []agentSpec{singleAgentFromLegacyEnv()}, nil
	}

	var specs []agentSpec
	seenName := make(map[string]bool)
	seenAppID := make(map[string]bool)

	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue // 容忍尾部空条目
		}

		spec, err := parseAgentEntry(entry)
		if err != nil {
			return nil, err
		}

		// 重名检查：同一 agent 名出现两次会让执行层选择不确定。
		if seenName[spec.Name] {
			return nil, fmt.Errorf(
				"%s 配置错误：agent 名 %q 重复——"+
					"会导致执行层选择歧义", envAgentsKey, spec.Name)
		}
		seenName[spec.Name] = true

		// app_id 跨 agent 重复：同一条消息（Header.AppID）会命中两个 agent，
		// 分流结果不确定。
		if seenAppID[spec.AppID] {
			return nil, fmt.Errorf(
				"%s 配置错误：app_id %q 被多个 agent 使用——"+
					"同一应用的消息无法确定归属", envAgentsKey, spec.AppID)
		}
		seenAppID[spec.AppID] = true

		specs = append(specs, spec)
	}

	if len(specs) == 0 {
		return nil, fmt.Errorf("%s 配置错误：未解析出任何 agent", envAgentsKey)
	}
	return specs, nil
}

// parseAgentEntry 解析单条 agent 定义（逗号分隔字段）。
func parseAgentEntry(entry string) (agentSpec, error) {
	var spec agentSpec
	for _, kv := range strings.Split(entry, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return agentSpec{}, fmt.Errorf(
				"%s 配置错误：条目 %q 中的 %q 不是 key=value 形式",
				envAgentsKey, entry, kv)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "name":
			spec.Name = v
		case "app_id":
			spec.AppID = v
		case "app_secret":
			spec.AppSecret = v
		}
	}

	// 三字段都必填——缺任一项都无法建立「agent ↔ 飞书应用」的绑定。
	if spec.Name == "" {
		return agentSpec{}, fmt.Errorf(
			"%s 配置错误：条目 %q 缺少 name", envAgentsKey, entry)
	}
	if spec.AppID == "" {
		return agentSpec{}, fmt.Errorf(
			"%s 配置错误：agent %q 缺少 app_id", envAgentsKey, spec.Name)
	}
	if spec.AppSecret == "" {
		return agentSpec{}, fmt.Errorf(
			"%s 配置错误：agent %q 缺少 app_secret", envAgentsKey, spec.Name)
	}
	return spec, nil
}

// singleAgentFromLegacyEnv 从旧环境变量构造单 agent（向后兼容路径）。
func singleAgentFromLegacyEnv() agentSpec {
	return agentSpec{
		Name:      defaultAgentName,
		AppID:     strings.TrimSpace(os.Getenv("FEISHU_APP_ID")),
		AppSecret: strings.TrimSpace(os.Getenv("FEISHU_APP_SECRET")),
	}
}
