package channel

import (
	"strings"
	"testing"
)

// 触发门禁契约（issue #6，设计文档 §4.4.3）。
//
// 两个正交维度：audience（谁能触发）× activation（何时触发）。
// 判定必须 fail-closed —— 关键元数据缺失时拒绝，不是放行。
//
// 六步优先级（§4.4.3）：
//   1. disabled 硬停止   2. 非群聊放行      3. always 放行
//   4. botOpenID 未知→拒  5. 未 @ bot→拒     6. owner_only 且非 owner→拒

func TestEvaluateGate_DisabledIsHardStop(t *testing.T) {
	// AC-1：disabled 时即使被 @ 也拒绝。
	d := EvaluateGate(GateInput{
		Activation: ActivationDisabled,
		ChatType:   ChatGroup,
		BotOpenID:  "ou_bot",
		Mentions:   []Mention{{OpenID: "ou_bot"}}, // 已被 @
		Audience:   AudienceEveryone,
	})
	if d.Allow {
		t.Fatal("activation=disabled must reject even when mentioned")
	}
	if d.Reason != ReasonActivationDisabled {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonActivationDisabled)
	}
}

func TestEvaluateGate_DisabledBeatsPrivateChat(t *testing.T) {
	// disabled 是第 1 步，优先于"非群聊放行"（第 2 步）。
	d := EvaluateGate(GateInput{
		Activation: ActivationDisabled,
		ChatType:   ChatDirect,
	})
	if d.Allow {
		t.Error("disabled must reject in private chat too (hard stop precedes private-chat allow)")
	}
}

func TestEvaluateGate_PrivateChatAllowed(t *testing.T) {
	// 第 2 步：非群聊放行（私聊无 @ 概念）。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatDirect,
		Audience:   AudienceEveryone,
	})
	if !d.Allow {
		t.Errorf("private chat should be allowed, got reason=%q", d.Reason)
	}
}

func TestEvaluateGate_AlwaysAllowedInGroup(t *testing.T) {
	// 第 3 步：always 放行（群聊里也无需 @）。
	d := EvaluateGate(GateInput{
		Activation: ActivationAlways,
		ChatType:   ChatGroup,
		Audience:   AudienceEveryone,
		BotOpenID:  "", // 即使 botOpenID 未知，always 也放行（第 3 步早于第 4 步）
	})
	if !d.Allow {
		t.Errorf("activation=always should allow, got reason=%q", d.Reason)
	}
}

func TestEvaluateGate_MissingBotOpenIDFailsClosed(t *testing.T) {
	// AC-2（血泪教训）：botOpenID 未知 + 群消息 → 拒绝。
	// 这是 happyclaw 历史 fail-open bug 的固化。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		BotOpenID:  "", // 未知
		Mentions:   []Mention{{OpenID: "ou_someone"}},
		Audience:   AudienceEveryone,
	})
	if d.Allow {
		t.Fatal("missing botOpenID in group must fail-closed (reject), not allow")
	}
	if d.Reason != ReasonBotOpenIDMissing {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonBotOpenIDMissing)
	}
}

func TestEvaluateGate_TextMentionWithoutMetadataIsRejected(t *testing.T) {
	// AC-3：消息文本含 "@bot" 但 mentions 元数据为空 → 拒绝。
	// 不得用文本匹配判断 @。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		BotOpenID:  "ou_bot",
		Mentions:   nil, // 元数据没有
		Audience:   AudienceEveryone,
	})
	if d.Allow {
		t.Fatal("text-only @ must not count as mention (must use metadata)")
	}
	if d.Reason != ReasonNotMentioned {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonNotMentioned)
	}
}

func TestEvaluateGate_MentionedByMetadataIsAllowed(t *testing.T) {
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		BotOpenID:  "ou_bot",
		Mentions:   []Mention{{OpenID: "ou_bot", Key: "@_user_1", Name: "Taiji"}},
		Audience:   AudienceEveryone,
	})
	if !d.Allow {
		t.Errorf("metadata mention should allow, got reason=%q", d.Reason)
	}
}

func TestEvaluateGate_OwnerOnlyRejectsNonOwner(t *testing.T) {
	// AC-4：owner_only 且 sender 非 owner → 拒绝。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		BotOpenID:  "ou_bot",
		Mentions:   []Mention{{OpenID: "ou_bot"}},
		Audience:   AudienceOwnerOnly,
		SenderID:   "ou_random_user",
		Owners:     []string{"ou_owner"},
	})
	if d.Allow {
		t.Fatal("owner_only with non-owner sender must reject")
	}
	if d.Reason != ReasonNotOwner {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonNotOwner)
	}
}

func TestEvaluateGate_OwnerOnlyWithEmptyOwnerListRejects(t *testing.T) {
	// AC-4（有意分歧）：owner 列表为空时必须拒绝，不得因"无 owner 信息"放行。
	// 这是对 happyclaw checkOwnerActive fail-open 行为的显式偏离。
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		BotOpenID:  "ou_bot",
		Mentions:   []Mention{{OpenID: "ou_bot"}},
		Audience:   AudienceOwnerOnly,
		SenderID:   "ou_anyone",
		Owners:     nil, // 无 owner 信息
	})
	if d.Allow {
		t.Fatal("owner_only with empty owner list must reject (fail-closed), not allow")
	}
	if d.Reason != ReasonNotOwner {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonNotOwner)
	}
}

func TestEvaluateGate_OwnerOnlyAcceptsOwner(t *testing.T) {
	d := EvaluateGate(GateInput{
		Activation: ActivationWhenMentioned,
		ChatType:   ChatGroup,
		BotOpenID:  "ou_bot",
		Mentions:   []Mention{{OpenID: "ou_bot"}},
		Audience:   AudienceOwnerOnly,
		SenderID:   "ou_owner",
		Owners:     []string{"ou_owner"},
	})
	if !d.Allow {
		t.Errorf("owner should be allowed, got reason=%q", d.Reason)
	}
}

func TestEvaluateGate_EveryReasonIsNonEmpty(t *testing.T) {
	// 拒绝必须带可区分的 reason，否则排障无法知道"为什么没响应"。
	cases := []GateInput{
		{Activation: ActivationDisabled},
		{Activation: ActivationWhenMentioned, ChatType: ChatGroup, BotOpenID: ""},
		{Activation: ActivationWhenMentioned, ChatType: ChatGroup, BotOpenID: "b"},
		{Activation: ActivationWhenMentioned, ChatType: ChatGroup, BotOpenID: "b",
			Mentions: []Mention{{OpenID: "b"}}, Audience: AudienceOwnerOnly},
	}
	for i, in := range cases {
		d := EvaluateGate(in)
		if d.Allow {
			t.Errorf("case %d should reject", i)
			continue
		}
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("case %d rejected without a reason", i)
		}
	}
}
