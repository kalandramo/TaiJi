package channel

import (
	"strings"
	"testing"
)

// ===== ChannelPrefix：渠道前缀（AC-1 的基础） =====

func TestChannelPrefix_KnownPlatforms(t *testing.T) {
	// 对齐 channel-prefixes.ts:2-11 的 CHANNEL_PREFIXES。
	// 前缀必须带分隔符，否则 "feishu"+"ws1" 与 "feish"+"uws1" 会撞。
	if got := ChannelPrefix(PlatformFeishu); got != "feishu:" {
		t.Errorf("ChannelPrefix(feishu) = %q, want %q", got, "feishu:")
	}
	if got := ChannelPrefix(Platform("telegram")); got != "telegram:" {
		t.Errorf("ChannelPrefix(telegram) = %q, want %q", got, "telegram:")
	}
}

func TestChannelPrefix_UnknownNonEmptyPlatformKeepsIsolation(t *testing.T) {
	// 未注册但非空的平台不能退化成空前缀——否则与其它未知渠道撞车。
	// 直接以平台名做前缀，保证「不同平台 → 不同前缀」这一不变量恒成立。
	got := ChannelPrefix(Platform("discord"))
	if got != "discord:" {
		t.Errorf("ChannelPrefix(discord) = %q, want %q（未注册平台也应保持隔离）", got, "discord:")
	}
}

func TestChannelPrefix_EmptyPlatformFailsClosed(t *testing.T) {
	// 空平台是解析层的编程错误（ParseCallback 必定设置 Platform）。
	// 返回空串会与「空前缀渠道」撞车；返回固定哨兵值使其在审计中可见且不撞真实渠道。
	got := ChannelPrefix("")
	if got == "" {
		t.Fatal("empty platform must not produce empty prefix (would collide with a prefix-less channel)")
	}
	if !strings.HasSuffix(got, ":") {
		t.Errorf("ChannelPrefix(\"\") = %q, want a sentinel ending in ':'", got)
	}
}

// ===== AC-1：渠道前缀隔离（Demo path ①） =====

func TestResolveRoute_ChannelPrefixIsolatesSameChatID(t *testing.T) {
	// Demo path ①：同 chatID、不同渠道 → 必须落到不同会话。
	// 这是「渠道前缀防跨渠道撞车」的核心断言。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingSingleContext}

	feishu := &IncomingMessage{Platform: PlatformFeishu, ChatID: "chatA", ChatType: ChatDirect}
	telegram := &IncomingMessage{Platform: Platform("telegram"), ChatID: "chatA", ChatType: ChatDirect}

	rf := ResolveRoute(cfg, feishu)
	rt := ResolveRoute(cfg, telegram)

	if rf.EffectiveJID == rt.EffectiveJID {
		t.Fatalf("same chatID on different channels must not share a session: both = %q", rf.EffectiveJID)
	}
	if rf.EffectiveJID != "feishu:ws1#chatA" {
		t.Errorf("feishu effectiveJID = %q, want %q", rf.EffectiveJID, "feishu:ws1#chatA")
	}
	if rt.EffectiveJID != "telegram:ws1#chatA" {
		t.Errorf("telegram effectiveJID = %q, want %q", rt.EffectiveJID, "telegram:ws1#chatA")
	}
}

func TestResolveRoute_SameChannelSameChatSharesSession(t *testing.T) {
	// 反向对照：同渠道同 chatID 必须落到**同一**会话。
	// 若前缀逻辑写坏成「每条消息唯一」，上面的隔离测试仍会通过——故需要此对照。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingSingleContext}
	a := ResolveRoute(cfg, &IncomingMessage{Platform: PlatformFeishu, ChatID: "chatA", ChatType: ChatDirect})
	b := ResolveRoute(cfg, &IncomingMessage{Platform: PlatformFeishu, ChatID: "chatA", ChatType: ChatDirect})
	if a.EffectiveJID != b.EffectiveJID {
		t.Errorf("same channel+chatID must share session: %q vs %q", a.EffectiveJID, b.EffectiveJID)
	}
}

// ===== AC-2：话题隔离（Demo path ②） =====

func TestResolveRoute_ThreadsAreIsolated(t *testing.T) {
	// Demo path ②：同一群两个不同话题 → 两个独立会话。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingThreadMap}

	mk := func(ctxID, rootID string) *IncomingMessage {
		return &IncomingMessage{
			Platform: PlatformFeishu,
			ChatID:   "oc_group",
			ChatType: ChatGroup,
			Meta: &ChannelMessageMeta{
				NativeContextType: "thread",
				ContextID:         ctxID,
				RootID:            rootID,
			},
		}
	}

	t1 := ResolveRoute(cfg, mk("thread_1", "root_1"))
	t2 := ResolveRoute(cfg, mk("thread_2", "root_2"))

	// 「看不到历史」的结构性保证：会话以 effectiveJID 为键，键不同则历史不共享。
	if t1.EffectiveJID == t2.EffectiveJID {
		t.Fatalf("two threads in one group must not share a session: both = %q", t1.EffectiveJID)
	}
	if t1.AgentID == t2.AgentID {
		t.Errorf("each thread must own a distinct agent: both = %q", t1.AgentID)
	}
	want1 := "feishu:ws1#oc_group#thread:thread_1#root:root_1"
	if t1.EffectiveJID != want1 {
		t.Errorf("thread effectiveJID = %q, want %q", t1.EffectiveJID, want1)
	}
}

func TestResolveRoute_SameThreadIsStable(t *testing.T) {
	// 同一话题的两条消息必须解析到同一会话与同一 agent（否则每来一条消息就开新会话）。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingThreadMap}
	mk := func() *IncomingMessage {
		return &IncomingMessage{
			Platform: PlatformFeishu, ChatID: "oc_group", ChatType: ChatGroup,
			Meta: &ChannelMessageMeta{NativeContextType: "thread", ContextID: "t1", RootID: "r1"},
		}
	}
	a, b := ResolveRoute(cfg, mk()), ResolveRoute(cfg, mk())
	if a.EffectiveJID != b.EffectiveJID || a.AgentID != b.AgentID {
		t.Errorf("same thread must be stable: (%q,%q) vs (%q,%q)", a.EffectiveJID, a.AgentID, b.EffectiveJID, b.AgentID)
	}
}

// ===== AC-3：保守判定——非 thread 不分流 =====

func TestResolveRoute_NonThreadDoesNotSplit(t *testing.T) {
	// 仅显式 nativeContextType == "thread" 才分流。
	// 否则每条顶层消息都开新会话（happyclaw channel-inbound-routing.ts:82-85 的教训）。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingThreadMap}
	base := "feishu:ws1#oc_group"

	for _, nct := range []string{"", "reply", "chat", "topic"} {
		in := &IncomingMessage{
			Platform: PlatformFeishu, ChatID: "oc_group", ChatType: ChatGroup,
			// 注意：ContextID 故意填了值——证明分流判据是 NativeContextType，不是 ContextID 是否存在。
			Meta: &ChannelMessageMeta{NativeContextType: nct, ContextID: "c1", RootID: "r1"},
		}
		got := ResolveRoute(cfg, in)
		if got.EffectiveJID != base {
			t.Errorf("nativeContextType=%q must not split: got %q, want %q", nct, got.EffectiveJID, base)
		}
		if got.AgentID != "" {
			t.Errorf("nativeContextType=%q must not allocate a thread agent: got %q", nct, got.AgentID)
		}
	}
}

func TestResolveRoute_ThreadMetadataWithoutThreadFlagDoesNotSplit(t *testing.T) {
	// 反向对照：有 thread 元数据但未标记 thread → 仍不分流。
	// 与上一个测试互补，共同锁定「判据是 NativeContextType」。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingThreadMap}
	in := &IncomingMessage{
		Platform: PlatformFeishu, ChatID: "oc_group", ChatType: ChatGroup,
		Meta: &ChannelMessageMeta{NativeContextType: "", ContextID: "c1"},
	}
	if got := ResolveRoute(cfg, in); got.EffectiveJID != "feishu:ws1#oc_group" {
		t.Errorf("got %q, want base session", got.EffectiveJID)
	}
}

func TestResolveRoute_SingleContextModeNeverSplits(t *testing.T) {
	// single_context 绑定模式下，即使有 thread 元数据也整群共用一个会话。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingSingleContext}
	in := &IncomingMessage{
		Platform: PlatformFeishu, ChatID: "oc_group", ChatType: ChatGroup,
		Meta: &ChannelMessageMeta{NativeContextType: "thread", ContextID: "t1", RootID: "r1"},
	}
	if got := ResolveRoute(cfg, in); got.EffectiveJID != "feishu:ws1#oc_group" {
		t.Errorf("single_context must not split: got %q", got.EffectiveJID)
	}
}

func TestResolveRoute_DirectChatNeverSplits(t *testing.T) {
	// 私聊没有话题概念；即便元数据带 thread 也不分流。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingThreadMap}
	in := &IncomingMessage{
		Platform: PlatformFeishu, ChatID: "ou_user", ChatType: ChatDirect,
		Meta: &ChannelMessageMeta{NativeContextType: "thread", ContextID: "t1"},
	}
	if got := ResolveRoute(cfg, in); got.EffectiveJID != "feishu:ws1#ou_user" {
		t.Errorf("direct chat must not split: got %q", got.EffectiveJID)
	}
}

// ===== AC-4：话题标识归一化优先级 =====

func TestResolveThread_Priority(t *testing.T) {
	// 优先级：contextId → threadId → rootId → messageId
	// 对齐 happyclaw channel-native-context.ts:22-38。
	cases := []struct {
		name string
		meta ChannelMessageMeta
		want string
	}{
		{"contextId wins", ChannelMessageMeta{ContextID: "c", ThreadID: "t", RootID: "r", MessageID: "m"}, "c"},
		{"threadId when no contextId", ChannelMessageMeta{ThreadID: "t", RootID: "r", MessageID: "m"}, "t"},
		{"rootId when no contextId/threadId", ChannelMessageMeta{RootID: "r", MessageID: "m"}, "r"},
		{"messageId as last resort", ChannelMessageMeta{MessageID: "m"}, "m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveThread(&tc.meta)
			if got == nil {
				t.Fatalf("ResolveThread = nil, want ContextID %q", tc.want)
			}
			if got.ContextID != tc.want {
				t.Errorf("ContextID = %q, want %q", got.ContextID, tc.want)
			}
		})
	}
}

func TestResolveThread_RootMessageIDFallsBackToContextID(t *testing.T) {
	// RootMessageID = rootId || contextId（对齐 channel-native-context.ts:41-47）
	got := ResolveThread(&ChannelMessageMeta{ContextID: "c"})
	if got == nil {
		t.Fatal("ResolveThread = nil")
	}
	if got.RootMessageID != "c" {
		t.Errorf("RootMessageID = %q, want fallback %q", got.RootMessageID, "c")
	}
	// 有 rootId 时以 rootId 为准
	got2 := ResolveThread(&ChannelMessageMeta{ContextID: "c", RootID: "r"})
	if got2.RootMessageID != "r" {
		t.Errorf("RootMessageID = %q, want %q", got2.RootMessageID, "r")
	}
}

func TestResolveThread_AllEmptyReturnsNil(t *testing.T) {
	// 全空 → nil（不分流）。nil meta 同样返回 nil。
	if got := ResolveThread(&ChannelMessageMeta{}); got != nil {
		t.Errorf("all-empty meta must return nil, got %+v", got)
	}
	if got := ResolveThread(nil); got != nil {
		t.Errorf("nil meta must return nil, got %+v", got)
	}
}

func TestResolveThread_TitleCap(t *testing.T) {
	// 标题上限 48 字符（按 rune 计，避免中文被截半）。
	long := strings.Repeat("话", 60)
	got := ResolveThread(&ChannelMessageMeta{ContextID: "c", Title: long})
	if got == nil {
		t.Fatal("ResolveThread = nil")
	}
	if n := len([]rune(got.Title)); n != 48 {
		t.Errorf("title rune length = %d, want 48", n)
	}

	// Title 为空时回退到 Text
	got2 := ResolveThread(&ChannelMessageMeta{ContextID: "c", Text: "hello"})
	if got2.Title != "hello" {
		t.Errorf("title = %q, want fallback to Text %q", got2.Title, "hello")
	}
}

// ===== AC-5：路由三元组形状 =====

func TestResolveRoute_ThreeTupleShape(t *testing.T) {
	// 路由结果必须含 effectiveJID / agentID / sourceJID 三项；
	// sourceJID 保留**原始**会话标识（无话题后缀）用于审计。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingThreadMap}
	in := &IncomingMessage{
		Platform: PlatformFeishu, ChatID: "oc_group", ChatType: ChatGroup,
		Meta: &ChannelMessageMeta{NativeContextType: "thread", ContextID: "t1", RootID: "r1"},
	}
	got := ResolveRoute(cfg, in)

	if got.EffectiveJID == "" || got.AgentID == "" || got.SourceJID == "" {
		t.Fatalf("all three fields must be populated: %+v", got)
	}
	if got.SourceJID != "feishu:ws1#oc_group" {
		t.Errorf("sourceJID = %q, want the raw session id without thread suffix", got.SourceJID)
	}
	if got.SourceJID == got.EffectiveJID {
		t.Error("sourceJID must preserve the pre-routing identity, not echo effectiveJID")
	}
	if !strings.HasPrefix(got.EffectiveJID, got.SourceJID) {
		t.Errorf("effectiveJID %q must extend sourceJID %q", got.EffectiveJID, got.SourceJID)
	}
}

func TestResolveRoute_NonThreadSourceJIDEqualsEffective(t *testing.T) {
	// 无话题时没有「路由后」的差异，两者相等——这也是 AC-5 的边界。
	cfg := RouteConfig{WorkspaceID: "ws1", BindingMode: BindingSingleContext}
	got := ResolveRoute(cfg, &IncomingMessage{Platform: PlatformFeishu, ChatID: "c", ChatType: ChatDirect})
	if got.SourceJID != got.EffectiveJID {
		t.Errorf("without routing, sourceJID (%q) must equal effectiveJID (%q)", got.SourceJID, got.EffectiveJID)
	}
	if got.AgentID != "" {
		t.Errorf("non-thread message must not get an agent id, got %q", got.AgentID)
	}
}
