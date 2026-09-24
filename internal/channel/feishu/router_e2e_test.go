package feishu

import (
	"testing"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 端到端路由验收（issue #7）。
//
// **驱动方式已从 webhook 改为直构 IncomingMessage**：原型只用长连接，
// webhook 实现（含 ParseCallback）已删除。长连接路径由 SDK 回调直接
// 产出 IncomingMessage（longconn.go），故这里直接构造——与真实形态一致。
//
// 验证内容不变：渠道前缀隔离、话题分流、无 thread 时的保守判定，
// 以及 nativeContextType 的取值契约（解析层只填 ThreadID，ContextID 恒空）。

// threadGroupMsg 构造带 thread_id 的群消息（长连接路径产物）。
func threadGroupMsg(threadID, rootID, text string) *channel.IncomingMessage {
	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    "ou_sender",
		ChatID:    "oc_group",
		ChatType:  channel.ChatGroup,
		MessageID: "om_1",
		Content:   text,
		Meta: &channel.ChannelMessageMeta{
			Provider: string(channel.PlatformFeishu),
			ChatType: "group",
			// 关键：解析层填 ThreadID 而非 ContextID（飞书用 thread_id）。
			// nativeContextType 只在有 thread_id 时返回 "thread"。
			NativeContextType: nativeContextType(threadID),
			ThreadID:          threadID,
			RootID:            rootID,
			MessageID:         "om_1",
			Text:              text,
		},
	}
}

// TestDemoPath_RouteEndToEnd 复现 issue #7 的两条 Demo path。
func TestDemoPath_RouteEndToEnd(t *testing.T) {
	cfg := channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap}

	t.Run("① 同 chatID 不同渠道 → 不同会话", func(t *testing.T) {
		feishuMsg := threadGroupMsg("", "", "hello")

		other := &channel.IncomingMessage{
			Platform: channel.Platform("telegram"),
			ChatID:   feishuMsg.ChatID, // 同 chatID
			ChatType: channel.ChatDirect,
		}
		rf := channel.ResolveRoute(cfg, feishuMsg)
		rt := channel.ResolveRoute(cfg, other)

		if rf.EffectiveJID == rt.EffectiveJID {
			t.Fatalf("same chatID across channels must differ: both = %q", rf.EffectiveJID)
		}
		if rf.EffectiveJID != "feishu:ws1#oc_group" {
			t.Errorf("feishu route = %q, want %q", rf.EffectiveJID, "feishu:ws1#oc_group")
		}
		if rt.EffectiveJID != "telegram:ws1#oc_group" {
			t.Errorf("telegram route = %q, want %q", rt.EffectiveJID, "telegram:ws1#oc_group")
		}
	})

	t.Run("② 同群两个话题 → 两个独立会话", func(t *testing.T) {
		m1 := threadGroupMsg("th_1", "root_1", "话题一")
		m2 := threadGroupMsg("th_2", "root_2", "话题二")

		r1 := channel.ResolveRoute(cfg, m1)
		r2 := channel.ResolveRoute(cfg, m2)

		if r1.EffectiveJID == r2.EffectiveJID {
			t.Fatalf("two threads must not share a session: both = %q", r1.EffectiveJID)
		}
		if r1.EffectiveJID != "feishu:ws1#oc_group#thread:th_1#root:root_1" {
			t.Errorf("thread1 route = %q", r1.EffectiveJID)
		}
		if r2.EffectiveJID != "feishu:ws1#oc_group#thread:th_2#root:root_2" {
			t.Errorf("thread2 route = %q", r2.EffectiveJID)
		}
		// sourceJID 相同（同一群），证明隔离来自话题后缀而非群标识差异。
		if r1.SourceJID != r2.SourceJID {
			t.Errorf("sourceJID should be the same group: %q vs %q", r1.SourceJID, r2.SourceJID)
		}
		if r1.AgentID == r2.AgentID {
			t.Errorf("threads must own distinct agents: both = %q", r1.AgentID)
		}
	})

	t.Run("③ 无 thread_id 的群消息不分流", func(t *testing.T) {
		m := threadGroupMsg("", "", "普通消息")
		if m.Meta == nil {
			t.Fatal("Meta must be populated")
		}
		if m.Meta.NativeContextType != "" {
			t.Errorf("NativeContextType = %q, want empty for a non-thread message", m.Meta.NativeContextType)
		}
		got := channel.ResolveRoute(cfg, m)
		if got.EffectiveJID != "feishu:ws1#oc_group" {
			t.Errorf("route = %q, want plain group session", got.EffectiveJID)
		}
		if got.AgentID != "" {
			t.Errorf("non-thread must not get an agent, got %q", got.AgentID)
		}
	})
}

// TestRoute_ParserFeedsThreadIdentity 锁定解析层与路由层的字段契约。
//
// 解析层填 ThreadID 而非 ContextID（飞书用 thread_id）。归一化优先级链的
// 第二档（threadId）因此在真实链路上生效——这个测试防止有人「顺手」把
// 解析层改成填 ContextID 而破坏跨平台契约的一致性。
func TestRoute_ParserFeedsThreadIdentity(t *testing.T) {
	m := threadGroupMsg("th_9", "root_9", "x")
	if m.Meta.ThreadID != "th_9" {
		t.Errorf("Meta.ThreadID = %q, want %q", m.Meta.ThreadID, "th_9")
	}
	if m.Meta.RootID != "root_9" {
		t.Errorf("Meta.RootID = %q, want %q", m.Meta.RootID, "root_9")
	}
	if m.Meta.ContextID != "" {
		t.Errorf("Meta.ContextID = %q, want empty (feishu uses thread_id)", m.Meta.ContextID)
	}
	// 归一化应取 threadId 档，而非 messageId 档。
	th := channel.ResolveThread(m.Meta)
	if th == nil {
		t.Fatal("ResolveThread = nil")
	}
	if th.ContextID != "th_9" {
		t.Errorf("normalized ContextID = %q, want %q (threadId tier)", th.ContextID, "th_9")
	}
}
