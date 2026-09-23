package authz

import "fmt"

// 主体解析与 owner 判定（issue #6，设计文档 §4.4.5）。
//
// 两条不变量：
//  1. 主体 ID 取自平台元数据（飞书 open_id），**不由调用方参数决定**。
//     反例是 trpc 的 preAuthIdentityMiddleware——它从 HTTP header 读 userID，
//     任何调用方都能声称自己是任何人。
//  2. owner 判定 fail-closed：owner 列表空 / sender 空 → 拒绝。
//     这是对 happyclaw checkOwnerActive「无 owner 信息即放行」的显式偏离。

// AudienceMode 决定谁能触发。
//
// 定义在本包（而非 channel）的理由：它是权限概念，被门禁（channel）与
// 主体解析共同消费；放权限层避免两处重复定义。
type AudienceMode string

const (
	// AudienceEveryone 所有允许成员均可触发。
	AudienceEveryone AudienceMode = "everyone"
	// AudienceOwnerOnly 仅 owner 可触发。
	AudienceOwnerOnly AudienceMode = "owner_only"
)

// Principal 是一次请求的主体身份。
type Principal struct {
	// Type 是主体类别（IM 用户为 "im_user"）。
	Type string
	// ID 是规范化 ID，带渠道前缀防跨渠道冲突。
	// 形态对齐 WeKnora service.go:425 的 {tenant}:{channel}:{platform}:{user}。
	ID string
}

// Valid 表示该主体可用于权限判定（有 ID 才能比对 owner）。
func (p Principal) Valid() bool { return p.ID != "" }

// PrincipalInput 是主体解析的输入。
//
// 字段全部来自**平台元数据**，不由调用方自由填写——解析函数的职责就是
// 把这些元数据规范化，调用方无从"声称"自己是谁。
type PrincipalInput struct {
	// ChannelID 是渠道配置标识，用于 namespace 隔离。
	ChannelID string
	// Platform 是平台类型（飞书为 "feishu"）。
	Platform string
	// OpenID 是平台原生用户 ID（飞书为 open_id）。
	OpenID string
}

// ResolvePrincipal 把平台元数据规范化为 Principal。
//
// OpenID 为空时返回 ID 为空的主体（Valid() == false）——不构造"匿名主体"，
// 因为匿名主体会让 owner 比对失去依据，从而静默放行。
func ResolvePrincipal(in PrincipalInput) Principal {
	if in.OpenID == "" {
		return Principal{Type: "im_user", ID: ""}
	}
	return Principal{
		Type: "im_user",
		ID:   fmt.Sprintf("%s:%s:%s", in.ChannelID, in.Platform, in.OpenID),
	}
}

// IsSenderAllowedByAudience 判断发送者是否被 audience 策略允许。
//
// 对齐 happyclaw isSenderAllowedByAudience（im-audience-policy.ts:21-32），
// 但**有意偏离**其 fail-open 行为：owner 或 sender 为空时拒绝，而非放行。
func IsSenderAllowedByAudience(audience AudienceMode, ownerID, senderID string) bool {
	if audience == AudienceEveryone {
		return true
	}
	return senderID != "" && ownerID != "" && senderID == ownerID
}

// IsOwner 判断 sender 是否在 owner 列表中。
//
// fail-closed：owners 为空、或 senderID 为空时一律 false。
// 这是对 happyclaw checkOwnerActive「无 owner 信息即放行」的显式偏离——
// 新渠道不应因缺少配置而敞开。
func IsOwner(owners []string, senderID string) bool {
	if senderID == "" || len(owners) == 0 {
		return false
	}
	for _, o := range owners {
		if o == senderID {
			return true
		}
	}
	return false
}
