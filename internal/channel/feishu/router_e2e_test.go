package feishu

import (
	"testing"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 端到端路由验收（issue #7）。
//
// 与 router_test.go 的区别：那里直接构造 IncomingMessage；
// 这里用**真实飞书事件 JSON** 走 ParseCallback → ResolveRoute 全链路，
// 验证解析层与路由层的**接线**——尤其是 nativeContextType 的取值
// 是否真能驱动分流（解析层只填 ThreadID，ContextID 恒空）。

// threadGroupEventJSON 构造带 thread_id 的群消息事件。
// threadID 为空则产出普通群消息（无 thread_id 字段）。
func threadGroupEventJSON(threadID, rootID, text string) string {
	threadPart := ""
	if threadID != "" {
		threadPart = `"thread_id":"` + threadID + `",`
	}
	rootPart := ""
	if rootID != "" {
		rootPart = `"root_id":"` + rootID + `",`
	}
	return `{
		"header":{"token":"` + testToken + `","event_type":"im.message.receive_v1"},
		"event":{
			"message":{
				"message_id":"om_1","chat_id":"oc_group","chat_type":"group",
				"message_type":"text",
				` + threadPart + rootPart + `
				"content":"{\"text\":\"` + text + `\"}",
				"create_time":"1"
			},
			"sender":{"sender_id":{"open_id":"ou_sender"}}
		}
	}`
}

// parseOne 走 HTTP 边界拿到解析后的 IncomingMessage。
func parseOne(t *testing.T, body string) *channel.IncomingMessage {
	t.Helper()
	r := newRecorder(VerifyConfig{VerificationToken: testToken})
	if w := r.do(body); w.Code != 200 {
		t.Fatalf("handler status = %d, want 200 (body=%s)", w.Code, body)
	}
	if r.messageCount() != 1 {
		t.Fatalf("messageCount = %d, want 1", r.messageCount())
	}
	return r.messages[0]
}

// TestDemoPath_RouteEndToEnd 复现 issue #7 的两条 Demo path。
func TestDemoPath_RouteEndToEnd(t *testing.T) {
	cfg := channel.RouteConfig{WorkspaceID: "ws1", BindingMode: channel.BindingThreadMap}

	t.Run("① 同 chatID 不同渠道 → 不同会话", func(t *testing.T) {
		// 真实链路上飞书侧只能拿到 feishu 渠道；另一渠道用同 chatID 的构造消息
		// 代表「另一个平台解析出的同 chatID」。前缀差异是这里唯一的分野。
		feishuMsg := parseOne(t, threadGroupEventJSON("", "", "hello"))

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
		m1 := parseOne(t, threadGroupEventJSON("th_1", "root_1", "话题一"))
		m2 := parseOne(t, threadGroupEventJSON("th_2", "root_2", "话题二"))

		r1 := channel.ResolveRoute(cfg, m1)
		r2 := channel.ResolveRoute(cfg, m2)

		if r1.EffectiveJID == r2.EffectiveJID {
			t.Fatalf("two threads must not share a session: both = %q", r1.EffectiveJID)
		}
		// 会话隔离 = 历史隔离：以 effectiveJID 为键，键不同则看不到彼此历史。
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
		// 真实链路验证保守判定：飞书普通群消息没有 thread_id，
		// 解析层 nativeContextType 返回 ""，路由层据此走普通会话。
		m := parseOne(t, threadGroupEventJSON("", "", "普通消息"))
		if m.Meta == nil {
			t.Fatal("Meta must be populated by the parser")
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
	m := parseOne(t, threadGroupEventJSON("th_9", "root_9", "x"))
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
