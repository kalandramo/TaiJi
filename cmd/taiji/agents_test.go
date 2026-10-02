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

// 未配置 TAIJI_AGENTS → **报错**（2026-10-02 起，此前是回退读 FEISHU_APP_ID）。
//
// 为什么改为报错而非回退：FEISHU_APP_ID / FEISHU_APP_SECRET 已删除，
// 配置面收敛到 TAIJI_AGENTS 一条通道。缺配置时明确报错，
// 好过用空凭据启动（那会在连飞书时才失败，错误指向「app_id is invalid」
// 而非「你没配配置」，误导排查方向）。
//
// **同时锁住**：错误信息必须指出该写什么（否则用户不知道格式）。
func TestEnvAgents_UnsetIsError(t *testing.T) {
	t.Setenv("TAIJI_AGENTS", "")

	specs, err := parseAgents()
	if err == nil {
		t.Fatalf("未配置 TAIJI_AGENTS 应报错，却得到 %d 个 spec", len(specs))
	}
	// 错误信息要能指导用户——含变量名与写法示例。
	for _, want := range []string{"TAIJI_AGENTS", "name=", "app_id="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q 以便用户照抄，实际：%v", want, err)
		}
	}
}

// 旧的 FEISHU_APP_ID / FEISHU_APP_SECRET 设了也不再生效。
//
// 这不是「兼容层」，而是删除后的**明确语义**：那两个变量已完全移除，
// 设了它们不会让 parseAgents 成功——避免用户以为旧配置还能用。
func TestEnvAgents_LegacyEnvVarsNoLongerWork(t *testing.T) {
	t.Setenv("TAIJI_AGENTS", "")
	t.Setenv("FEISHU_APP_ID", "cli_legacy")
	t.Setenv("FEISHU_APP_SECRET", "sec_legacy")

	if _, err := parseAgents(); err == nil {
		t.Error("旧变量 FEISHU_APP_ID/SECRET 已删除，设了也不应让 parseAgents 成功")
	}
}

// 单 agent 与多 agent 用**同一个变量**——区别只在条目数。
func TestEnvAgents_SingleEntryIsValidSingleAgent(t *testing.T) {
	t.Setenv("TAIJI_AGENTS", "name=assistant,app_id=cli_one,app_secret=s1")

	specs, err := parseAgents()
	if err != nil {
		t.Fatalf("单条配置应解析成功: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("应得 1 个 spec，got %d", len(specs))
	}
	if specs[0].Name != "assistant" || specs[0].AppID != "cli_one" {
		t.Errorf("解析结果不符: %+v", specs[0])
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
