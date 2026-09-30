package main

import (
	"strings"
	"testing"
)

// TAIJI_AGENTS 解析（多 agent + 每 agent 独立飞书应用）。
//
// 格式：分号分隔 agent，逗号分隔字段：
//
//	TAIJI_AGENTS="name=billing,app_id=cli_aaa,app_secret=s1;name=ops,app_id=cli_bbb,app_secret=s2"
//
// 设计理由（与既有 TAIJI_RBAC 同形态）：单变量分号分隔条目，
// 条目数与 agent 数同步增删，不会出现「改了 A 忘了 B」的残配置。

func TestEnvAgents_UnsetFallsBackToSingle(t *testing.T) {
	t.Setenv("TAIJI_AGENTS", "")
	t.Setenv("FEISHU_APP_ID", "cli_single")
	t.Setenv("FEISHU_APP_SECRET", "sec_single")

	specs, err := parseAgents()
	if err != nil {
		t.Fatalf("未配置 TAIJI_AGENTS 不应报错: %v", err)
	}
	// 向后兼容：回退到单 agent（读旧变量）。
	if len(specs) != 1 {
		t.Fatalf("未配置时应回退为 1 个 agent，got %d", len(specs))
	}
	if specs[0].AppID != "cli_single" || specs[0].AppSecret != "sec_single" {
		t.Errorf("回退时应读 FEISHU_APP_ID/SECRET，got %+v", specs[0])
	}
	if specs[0].Name != defaultAgentName {
		t.Errorf("回退时 agent 名应为 %q，got %q", defaultAgentName, specs[0].Name)
	}
}

func TestEnvAgents_ParsesTwoAgents(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=billing,app_id=cli_aaa,app_secret=s1;"+
			"name=ops,app_id=cli_bbb,app_secret=s2")

	specs, err := parseAgents()
	if err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("应解析出 2 个 agent，got %d", len(specs))
	}

	byName := map[string]agentSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	if got := byName["billing"]; got.AppID != "cli_aaa" || got.AppSecret != "s1" {
		t.Errorf("billing 解析错误: %+v", got)
	}
	if got := byName["ops"]; got.AppID != "cli_bbb" || got.AppSecret != "s2" {
		t.Errorf("ops 解析错误: %+v", got)
	}
}

// 缺 app_secret 必须 fail-fast，且错误信息指出**哪个 agent**——
// 否则多 agent 配置下用户不知道是哪条写错了。
func TestEnvAgents_MissingSecretFailsFast(t *testing.T) {
	t.Setenv("TAIJI_AGENTS", "name=billing,app_id=cli_aaa;name=ops,app_id=cli_bbb,app_secret=s2")

	_, err := parseAgents()
	if err == nil {
		t.Fatal("缺 app_secret 应报错（fail-fast）")
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("错误信息应指出是哪个 agent 缺字段，got: %v", err)
	}
}

func TestEnvAgents_MissingNameFailsFast(t *testing.T) {
	t.Setenv("TAIJI_AGENTS", "app_id=cli_aaa,app_secret=s1")
	_, err := parseAgents()
	if err == nil {
		t.Fatal("缺 name 应报错（fail-fast）")
	}
}

// 重复 agent 名会导致分流歧义（同一 AppID 映射到哪个 agent 不确定）。
func TestEnvAgents_DuplicateNameFailsFast(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=dup,app_id=cli_aaa,app_secret=s1;"+
			"name=dup,app_id=cli_bbb,app_secret=s2")
	_, err := parseAgents()
	if err == nil {
		t.Fatal("重复 agent 名应报错（分流歧义）")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("错误信息应指出重复的名字，got: %v", err)
	}
}

// 跨 agent 的 app_id 重复会导致分流歧义（同一 AppID 命中两个 agent）。
func TestEnvAgents_DuplicateAppIDFailsFast(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"name=a,app_id=cli_same,app_secret=s1;"+
			"name=b,app_id=cli_same,app_secret=s2")
	_, err := parseAgents()
	if err == nil {
		t.Fatal("重复 app_id 应报错（同一 AppID 无法映射到两个 agent）")
	}
	if !strings.Contains(err.Error(), "cli_same") {
		t.Errorf("错误信息应指出重复的 app_id，got: %v", err)
	}
}

// 容忍空白与尾部空条目（与 envRBAC 的既有取向一致）。
func TestEnvAgents_ToleratesWhitespaceAndTrailingSemicolon(t *testing.T) {
	t.Setenv("TAIJI_AGENTS",
		"  name=billing , app_id=cli_aaa , app_secret=s1 ;  ")
	specs, err := parseAgents()
	if err != nil {
		t.Fatalf("应容忍空白: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("尾部空条目应被忽略，got %d 个", len(specs))
	}
	if specs[0].Name != "billing" || specs[0].AppID != "cli_aaa" {
		t.Errorf("空白未正确去除: %+v", specs[0])
	}
}

// 凭据保护：TAIJI_AGENTS 必须加入 ReservedKeys（内含 ak/sk）。
func TestAgents_TAIJI_AGENTSIsReserved(t *testing.T) {
	// 由 internal/config 的测试覆盖具体实现，此处断言常量名一致，
	// 防止两处拼写漂移。
	if strings.TrimSpace(envAgentsKey) != "TAIJI_AGENTS" {
		t.Errorf("envAgentsKey = %q，应为 TAIJI_AGENTS", envAgentsKey)
	}
}
