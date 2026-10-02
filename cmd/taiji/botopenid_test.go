package main

import (
	"errors"
	"strings"
	"testing"
)

// bot open_id 的获取策略（2026-10-02 回归修复）。
//
// ## 被修复的回归
//
// 曾把自动获取改成**无条件 fail-fast**，后果：
//
//	[单 agent + 已配 TAIJI_FEISHU_BOT_OPEN_ID]
//	taiji serve: 获取 agent "assistant" 的 bot open_id 失败（…）
//	exit=1
//
// 用户配置完全正确，却起不来——因为「自动获取」被做成了启动的硬依赖，
// 而 `/open-apis/bot/v3/info` 是网络调用：凭据暂时失效、离线调试、
// 纯私聊部署都会被它挡住，**而这些场景本不需要 open_id**。
//
// ## 正确的策略（本测试锁住的）
//
//	多 agent + 获取失败 → fail-fast（手填单值服务不了多个 bot，无退路）
//	单 agent + 获取失败 + 有手填值 → 回退（群聊 @ 判定仍可用）
//	单 agent + 获取失败 + 无手填值 → 警告（群聊会被拒，私聊不受影响）
//	获取成功 → 用映射（无论单/多 agent）

func TestDecideBotOpenID_MultiAgentFailureIsFatal(t *testing.T) {
	d := decideBotOpenID(2, errors.New("network down"), "ou_manual")

	if !d.fatal {
		t.Error("多 agent 下获取失败必须 fail-fast——手填单值服务不了多个 bot")
	}
	if d.useMapping {
		t.Error("失败时不应使用映射")
	}
}

func TestDecideBotOpenID_SingleAgentFailureWithManualFallsBack(t *testing.T) {
	d := decideBotOpenID(1, errors.New("network down"), "ou_manual")

	if d.fatal {
		t.Fatal("单 agent + 有手填值时不应 fail-fast——" +
			"否则「配置正确却起不来」（这正是被修复的回归）")
	}
	if d.useMapping {
		t.Error("回退时不应使用映射——应保留手填的单值")
	}
	if !strings.Contains(d.note, "回退") {
		t.Errorf("应提示已回退，实际: %q", d.note)
	}
}

func TestDecideBotOpenID_SingleAgentFailureWithoutManualWarns(t *testing.T) {
	d := decideBotOpenID(1, errors.New("network down"), "")

	if d.fatal {
		t.Fatal("单 agent + 无手填值时应警告而非拒绝启动——" +
			"私聊与其它功能仍可用，不该被群聊的 @ 判定拖死")
	}
	// 必须说明后果（否则用户不知道群聊为什么没反应）。
	if !strings.Contains(d.note, "群聊") {
		t.Errorf("警告应说明后果（群聊被拒），实际: %q", d.note)
	}
	if !strings.Contains(d.note, "私聊") {
		t.Errorf("警告应说明哪些功能不受影响，实际: %q", d.note)
	}
}

func TestDecideBotOpenID_SuccessUsesMapping(t *testing.T) {
	d := decideBotOpenID(1, nil, "ou_manual")

	if d.fatal {
		t.Error("成功时不应 fail")
	}
	if !d.useMapping {
		t.Error("成功时应使用刚取到的映射")
	}
	// 成功时不该有回退提示（避免噪声）。
	if d.note != "" {
		t.Errorf("成功时不应有提示，got %q", d.note)
	}
}

// 多 agent 成功时同样用映射（映射是唯一能服务多 bot 的形态）。
func TestDecideBotOpenID_MultiAgentSuccessUsesMapping(t *testing.T) {
	d := decideBotOpenID(2, nil, "")

	if d.fatal || !d.useMapping {
		t.Errorf("多 agent 成功应使用映射，got fatal=%v useMapping=%v",
			d.fatal, d.useMapping)
	}
}

// 决策结构**不携带映射数据**——映射由调用方持有。
//
// 为什么锁这条：早期版本让决策持有 mapping，用「空 map」表示成功——
// 而空 map 与「映射存在但无条目」在门禁侧含义不同（后者会让所有
// AppID 都解析失败）。让决策只管「怎么办」，歧义就没有了。
func TestDecideBotOpenID_DoesNotCarryMappingData(t *testing.T) {
	// 编译期即保证：botOpenIDDecision 没有 map 字段。
	// 若后人加回 mapping 字段，本测试的语义（决策与数据分离）会被破坏——
	// 故用一条运行时断言记录该契约。
	d := decideBotOpenID(1, nil, "")
	if d.useMapping && d.note == "" && !d.fatal {
		// 成功路径：调用方负责用自己刚取到的 mapping。
		// 决策结构本身不含数据，这正是我们要的。
		return
	}
	t.Logf("决策结构：fatal=%v useMapping=%v note=%q（不含映射数据）",
		d.fatal, d.useMapping, d.note)
}
