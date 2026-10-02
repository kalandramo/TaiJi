package channel

import "testing"

// 门禁的 per-agent bot open_id 解析（真实多 agent 同群暴露）。
//
// 背景：多 agent 部署下每个 bot 的 open_id 不同，而门禁原先只读一个
// 全局 BotOpenID。后果是「@ bot B」被判定为 not_mentioned——用户明明 @ 了，
// bot 却不响应，且日志把原因写成「未提及」，指向错误的排查方向。
//
// 真实证据：2026-10-02 的双 agent 同群运行日志
//   [pipeline] server: 分流 message_id=om_...app_id=cli_aa0110... agent=root
//   [pipeline] server: gate rejected message_id=om_... reason=not_mentioned
// 第二条消息的 app_id 属于 bill，而门禁用的是 root 的 open_id。

func TestEvaluateGate_BotOpenIDResolvedByAppID(t *testing.T) {
	// 两个 agent，各自的 open_id 不同。
	byAppID := map[string]string{
		"cli_root": "ou_root",
		"cli_bill": "ou_bill",
	}

	// bill 的 bot 被 @：必须放行。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		Audience:   AudienceEveryone,
		AppID:      "cli_bill",
		BotOpenIDs: byAppID,
		Mentions:   []Mention{{OpenID: "ou_bill"}},
	})
	if !d.Allow {
		t.Fatalf("bill 的 bot 被 @ 应放行，却被拒 reason=%q——"+
			"这说明门禁没按 app_id 取 bot open_id", d.Reason)
	}

	// root 的 bot 被 @：同样放行（换一个 app_id，结论必须跟着换）。
	d = EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		Audience:   AudienceEveryone,
		AppID:      "cli_root",
		BotOpenIDs: byAppID,
		Mentions:   []Mention{{OpenID: "ou_root"}},
	})
	if !d.Allow {
		t.Fatalf("root 的 bot 被 @ 应放行，却被拒 reason=%q", d.Reason)
	}
}

func TestEvaluateGate_OtherAgentsMentionDoesNotMatch(t *testing.T) {
	// 反向：@ 的是**别的** bot，本 bot 不该被触发。
	// 否则两个 bot 会同时回答同一条消息。
	byAppID := map[string]string{
		"cli_root": "ou_root",
		"cli_bill": "ou_bill",
	}

	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		Audience:   AudienceEveryone,
		AppID:      "cli_bill", // 本条消息投给 bill
		BotOpenIDs: byAppID,
		Mentions:   []Mention{{OpenID: "ou_root"}}, // 但 @ 的是 root
	})
	if d.Allow {
		t.Fatal("@ 的是别的 bot，本 bot 不应被触发（否则两个 bot 会同时抢答）")
	}
	if d.Reason != ReasonNotMentioned {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonNotMentioned)
	}
}

func TestEvaluateGate_UnknownAppIDFailsClosed(t *testing.T) {
	// 配置了映射表但本条消息的 app_id 不在表里——fail-closed。
	//
	// 这是装配缺陷（或平台投递了未注册的应用），静默放行会让
	// 未注册的 bot 也能触发 agent。与 resolveAgent 的取向一致。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		Audience:   AudienceEveryone,
		AppID:      "cli_unknown",
		BotOpenIDs: map[string]string{"cli_root": "ou_root"},
		Mentions:   []Mention{{OpenID: "ou_root"}},
	})
	if d.Allow {
		t.Fatal("未知 app_id 应 fail-closed，不得放行")
	}
	if d.Reason != ReasonBotOpenIDMissing {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonBotOpenIDMissing)
	}
}

func TestEvaluateGate_SingleAgentFallbackUnchanged(t *testing.T) {
	// 向后兼容不变量：未配映射表（单 agent 部署）时，行为与改动前**完全一致**
	// ——用全局 BotOpenID。这条松动会让既有部署静默失效。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		Audience:   AudienceEveryone,
		AppID:      "cli_whatever", // 单 agent 下 app_id 不参与判定
		BotOpenID:  "ou_single",    // 全局单值
		BotOpenIDs: nil,            // 未配映射
		Mentions:   []Mention{{OpenID: "ou_single"}},
	})
	if !d.Allow {
		t.Fatalf("单 agent 回退路径应放行（用全局 BotOpenID），却被拒 reason=%q", d.Reason)
	}
}

func TestEvaluateGate_MapConfiguredButEmptyValueFailsClosed(t *testing.T) {
	// 映射表存在但该 app_id 对应的值为空——视为「未配置」→ fail-closed。
	// 空串参与比对恒不成立，若不显式拒绝，会走到 isBotMentioned 的
	// 「空 botOpenID 恒 false」路径——结果相同，但 reason 会指向
	// not_mentioned 而非配置缺失，误导排查。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		Audience:   AudienceEveryone,
		AppID:      "cli_root",
		BotOpenIDs: map[string]string{"cli_root": ""},
		Mentions:   []Mention{{OpenID: "ou_root"}},
	})
	if d.Allow {
		t.Fatal("映射值为空应 fail-closed")
	}
	if d.Reason != ReasonBotOpenIDMissing {
		t.Errorf("reason = %q, want %q（应指向配置缺失而非未提及）",
			d.Reason, ReasonBotOpenIDMissing)
	}
}
