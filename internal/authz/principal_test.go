package authz

import (
	"strings"
	"testing"
)

// 主体解析契约（issue #6 AC-4，设计文档 §4.4.5）。
//
// 要求：
//   - 主体 ID 取自平台元数据（飞书 open_id），不由调用方参数决定
//   - ID 带渠道前缀防跨渠道冲突（对齐 WeKnora 的 {tenant}:{channel}:{platform}:{user}）
//   - owner 判定 fail-closed：owner 列表空 / sender 空 → 拒绝

func TestResolvePrincipal_UsesPlatformOpenID(t *testing.T) {
	// 主体 ID 必须来自平台元数据，不是调用方传的参数。
	p := ResolvePrincipal(PrincipalInput{
		ChannelID: "ch-1",
		Platform:  "feishu",
		OpenID:    "ou_sender1",
	})
	if p.Type != "im_user" {
		t.Errorf("Type = %q, want im_user", p.Type)
	}
	if !strings.Contains(p.ID, "ou_sender1") {
		t.Errorf("ID = %q, should contain the platform open_id", p.ID)
	}
}

func TestResolvePrincipal_IDCarriesChannelPrefix(t *testing.T) {
	// 不同渠道的同名 ID 不得冲突（namespace 隔离）。
	a := ResolvePrincipal(PrincipalInput{ChannelID: "chA", Platform: "feishu", OpenID: "u1"})
	b := ResolvePrincipal(PrincipalInput{ChannelID: "chB", Platform: "feishu", OpenID: "u1"})
	if a.ID == b.ID {
		t.Errorf("different channels produced the same principal ID %q (namespace leak)", a.ID)
	}
}

func TestResolvePrincipal_EmptyOpenIDIsRejected(t *testing.T) {
	// 无 open_id 时不得产出"匿名主体"——那会让 owner 比对失去依据。
	p := ResolvePrincipal(PrincipalInput{ChannelID: "ch", Platform: "feishu", OpenID: ""})
	if p.ID != "" {
		t.Errorf("empty open_id should yield empty principal ID, got %q", p.ID)
	}
	if p.Valid() {
		t.Error("principal with empty open_id should be invalid")
	}
}

func TestResolvePrincipal_ValidWhenOpenIDPresent(t *testing.T) {
	p := ResolvePrincipal(PrincipalInput{ChannelID: "ch", Platform: "feishu", OpenID: "ou_x"})
	if !p.Valid() {
		t.Error("principal with open_id should be valid")
	}
}

func TestIsSenderAllowedByAudience_EveryoneAllows(t *testing.T) {
	// audience=everyone 时无需比对 owner。
	if !IsSenderAllowedByAudience(AudienceEveryone, "", "anyone") {
		t.Error("everyone should allow any sender")
	}
}

func TestIsSenderAllowedByAudience_OwnerOnlyMatches(t *testing.T) {
	if !IsSenderAllowedByAudience(AudienceOwnerOnly, "ou_owner", "ou_owner") {
		t.Error("owner should be allowed")
	}
}

func TestIsSenderAllowedByAudience_OwnerOnlyRejectsOthers(t *testing.T) {
	if IsSenderAllowedByAudience(AudienceOwnerOnly, "ou_owner", "ou_other") {
		t.Error("non-owner should be rejected")
	}
}

func TestIsSenderAllowedByAudience_EmptyOwnerFailsClosed(t *testing.T) {
	// 有意偏离 happyclaw 的 fail-open：无 owner 信息时拒绝。
	if IsSenderAllowedByAudience(AudienceOwnerOnly, "", "ou_anyone") {
		t.Error("owner_only with empty owner must reject (fail-closed)")
	}
}

func TestIsSenderAllowedByAudience_EmptySenderRejected(t *testing.T) {
	if IsSenderAllowedByAudience(AudienceOwnerOnly, "ou_owner", "") {
		t.Error("empty sender must be rejected")
	}
}

func TestIsOwner_EmptyOwnerListFailsClosed(t *testing.T) {
	if IsOwner(nil, "ou_anyone") {
		t.Error("empty owner list must reject")
	}
	if IsOwner([]string{}, "ou_anyone") {
		t.Error("empty owner list must reject")
	}
}

func TestIsOwner_Matches(t *testing.T) {
	owners := []string{"ou_a", "ou_b"}
	if !IsOwner(owners, "ou_b") {
		t.Error("listed owner should match")
	}
	if IsOwner(owners, "ou_c") {
		t.Error("unlisted sender should not match")
	}
	if IsOwner(owners, "") {
		t.Error("empty sender should not match")
	}
}
