// Package channel 的触发门禁（issue #6）。
//
// 在消息进入 agent 之前决定「该不该响应」。两个正交维度：
// audience（谁能触发）× activation（何时触发）。
//
// 安全取向是 fail-closed：关键元数据缺失时拒绝，不是放行。
// 依据设计文档 §4.4.3；其中第 4 条（botOpenID 未知即拒）是
// happyclaw 历史 fail-open bug 的固化（feishu-mention-gate.ts:8-10）。
package channel

import "github.com/kalandramo/TaiJi/internal/authz"

// AudienceMode 复用权限层的定义——避免同一概念两处维护而漂移。
// 依赖方向 channel → authz（单向，无环）。
type AudienceMode = authz.AudienceMode

const (
	// AudienceEveryone 所有允许成员均可触发。
	AudienceEveryone = authz.AudienceEveryone
	// AudienceOwnerOnly 仅 owner 可触发。
	AudienceOwnerOnly = authz.AudienceOwnerOnly
)

// ActivationMode 决定何时触发。
type ActivationMode string

const (
	// ActivationAlways 无需 @ 即可触发。
	ActivationAlways ActivationMode = "always"
	// ActivationWhenMentioned 需 @ bot。
	ActivationWhenMentioned ActivationMode = "when_mentioned"
	// ActivationDisabled 硬停止——即使被 @ 也不响应。
	ActivationDisabled ActivationMode = "disabled"
)

// 拒绝原因。每个拒绝都必须可区分，否则排障无从下手。
const (
	ReasonActivationDisabled = "activation_disabled"
	ReasonBotOpenIDMissing   = "bot_open_id_missing"
	ReasonNotMentioned       = "not_mentioned"
	ReasonNotOwner           = "not_owner"
)

// GateInput 是门禁判定所需的全部输入。
type GateInput struct {
	Audience   AudienceMode
	Activation ActivationMode

	// ChatType 决定是否走 @ 逻辑（私聊无 @ 概念）。
	ChatType ChatType

	// BotOpenID 是 bot 自身的平台 ID。群聊下未知 → 拒绝（fail-closed）。
	BotOpenID string

	// SenderID 是发送者的规范化 ID（owner 比对用）。
	SenderID string

	// Mentions 是平台元数据的 @ 列表。**必须用它判断 @，不得用文本匹配**——
	// 用户手写 @name 会被误判（§4.4.3）。
	Mentions []Mention

	// Owners 是允许的 owner 列表。owner_only 且此列表为空 → 拒绝。
	Owners []string
}

// Decision 是门禁结论。
type Decision struct {
	Allow  bool
	Reason string // 拒绝时非空
}

// EvaluateGate 按六步优先级判定。顺序即语义，不可调换。
//
// 1. disabled 硬停止（即使被 @ 也拒）
// 2. 非群聊放行（私聊无 @ 概念）
// 3. always 放行
// 4. botOpenID 未知 → 拒绝（fail-closed，血泪教训）
// 5. 未 @ bot → 拒绝
// 6. owner_only 且非 owner → 拒绝
func EvaluateGate(in GateInput) Decision {
	// 1. disabled 是硬停止，优先于一切（含"私聊放行"）。
	if in.Activation == ActivationDisabled {
		return reject(ReasonActivationDisabled)
	}

	// 2. 非群聊放行：私聊没有 @ 概念，也不涉及群内 audience 策略。
	if in.ChatType != ChatGroup {
		return allow()
	}

	// 3. always 放行：群内也无需 @。此步早于第 4 步，故 always 时
	//    botOpenID 未知不影响判定。
	if in.Activation == ActivationAlways {
		return allow()
	}

	// 4. botOpenID 未知 → 拒绝。
	//    这是 fail-closed 的核心：旧实现"安全降级=默认放行"导致
	//    require_mention 在所有群静默失效。宁可拒绝并告警，不可静默放行。
	if in.BotOpenID == "" {
		return reject(ReasonBotOpenIDMissing)
	}

	// 5. 未 @ bot → 拒绝。比对 mentions 元数据的 OpenID，
	//    绝不用文本匹配（用户手写 @name 会误判）。
	if !isBotMentioned(in.BotOpenID, in.Mentions) {
		return reject(ReasonNotMentioned)
	}

	// 6. owner_only 且非 owner → 拒绝。
	//    owner 列表为空时 IsOwner 返回 false → 拒绝（有意偏离 happyclaw 的 fail-open）。
	if in.Audience == AudienceOwnerOnly && !in.IsOwner(in.SenderID) {
		return reject(ReasonNotOwner)
	}

	return allow()
}

// IsOwner 判断 sender 是否为 owner。委托给权限层的实现，避免两处判定漂移。
//
// fail-closed：Owners 为空、或 sender 为空时一律 false（拒绝）。
// 这是对 happyclaw checkOwnerActive「无 owner 信息即放行」的显式偏离——
// 新渠道不应因缺少配置而敞开。
func (in GateInput) IsOwner(senderID string) bool {
	return authz.IsOwner(in.Owners, senderID)
}

// isBotMentioned 在 mentions 元数据里查找 bot 的 OpenID。
//
// 空 botOpenID 恒返回 false——调用方需自行决定 fail-closed 还是降级；
// 本包在 EvaluateGate 第 4 步已先行拒绝，故此处返回 false 是安全的。
func isBotMentioned(botOpenID string, mentions []Mention) bool {
	if botOpenID == "" {
		return false
	}
	for _, m := range mentions {
		if m.OpenID == botOpenID {
			return true
		}
	}
	return false
}

func allow() Decision               { return Decision{Allow: true} }
func reject(reason string) Decision { return Decision{Allow: false, Reason: reason} }
