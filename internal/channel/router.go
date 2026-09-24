// 会话路由：把入站消息映射到唯一的会话标识（effectiveJID）。
//
// 依据设计文档 §4.4.4（docs/03-原型设计文档.md:591）与 §4.4.1（:437）。
// 签名对齐 happyclaw resolveEffectiveChatJid（im-channel.ts:146-153），
// 返回 {effectiveJid, agentId, sourceJid} 三元组。
//
// 两条关键约束：
//
//  1. 渠道前缀防跨渠道撞车——feishu 与 telegram 的同名 chatID 必须落到不同会话
//     （channel-prefixes.ts:2-11）。
//  2. 话题保守判定——仅显式 nativeContextType == "thread" 才视为话题，
//     否则每条顶层消息都会开一个新会话（channel-inbound-routing.ts:82-85）。
package channel

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// BindingMode 是会话绑定模式（v1.4 的简化子集）。
//
// 设计文档 §4.4.4 末段声明：原型**不实现** v1.4 的完整「绑定模式 × 路由模式」
// 矩阵（2×2），只实现最常用的两种。
type BindingMode string

const (
	// BindingSingleContext 是整群共用一个会话。
	BindingSingleContext BindingMode = "single_context"
	// BindingThreadMap 是按话题分流——每个话题一个独立会话与 agent。
	BindingThreadMap BindingMode = "thread_map"
)

// RouteConfig 是路由配置。
type RouteConfig struct {
	// WorkspaceID 参与会话标识编码，使串行化键派生保持纯字符串解析、零 IO（§4.5）。
	WorkspaceID string
	// BindingMode 决定群聊是否按话题分流。
	BindingMode BindingMode
}

// RouteTarget 是路由结果。
type RouteTarget struct {
	// EffectiveJID 是路由后的会话标识，格式：
	//   {channelPrefix}{workspaceId}#{localSessionId}
	// 话题追加 #thread:{contextId}#root:{rootMessageId}。
	EffectiveJID string
	// AgentID 是话题独立 agent 的 ID；无话题时为空。
	AgentID string
	// SourceJID 是**原始**会话标识（无话题后缀），用于审计与串行化键派生。
	SourceJID string
}

// NativeThreadContext 是归一化后的话题标识。
type NativeThreadContext struct {
	ContextID     string
	RootMessageID string
	Title         string
}

// titleMaxRunes 是话题标题上限。按 rune 计，避免中文被截成半个字。
const titleMaxRunes = 48

// ChannelPrefix 返回渠道前缀（含分隔冒号）。
//
// 对齐 happyclaw CHANNEL_PREFIXES（channel-prefixes.ts:2-11）与
// getChannelFromJid（:14-19）——原型采纳「前缀即渠道标识」的同一约定。
//
// 冒号是**必需**的：否则 "feishu"+"ws1" 与 "feish"+"uws1" 会构造出同一标识。
func ChannelPrefix(p Platform) string {
	name := string(p)
	if name == "" {
		// 空平台是解析层的编程错误（ParseCallback 必设 Platform）。
		// 返回空串会与「无前缀渠道」撞车；返回固定哨兵使其在审计中可见且不撞真实渠道。
		name = "unknown"
	}
	return name + ":"
}

// ResolveThread 归一化话题标识；无话题信息时返回 nil。
//
// 优先级：contextId → threadId → rootId → messageId
// 对齐 happyclaw resolveNativeThreadContext（channel-native-context.ts:22-38）。
//
// 注意本函数只做**归一化**，不判定「是否话题」——那是 ResolveRoute 的职责
// （保守判定见 channel-inbound-routing.ts:82-85）。分开是刻意的：
// 归一化问「话题是什么」，判定问「算不算话题」，混在一起会让调用方无法复用前者。
func ResolveThread(meta *ChannelMessageMeta) *NativeThreadContext {
	if meta == nil {
		return nil
	}
	ctxID := firstNonEmpty(meta.ContextID, meta.ThreadID, meta.RootID, meta.MessageID)
	if ctxID == "" {
		return nil
	}
	return &NativeThreadContext{
		ContextID:     ctxID,
		RootMessageID: firstNonEmpty(meta.RootID, ctxID),
		Title:         summarizeTitle(firstNonEmpty(meta.Title, meta.Text)),
	}
}

// localSessionID 决定会话标识的本地部分。
//
// 群聊用 ChatID（群即会话）。私聊的 ChatID 为空（inbound.go:79），
// 此时必须用 UserID——否则**所有私聊用户会共用同一个会话**
// （`feishu:ws1#`），历史互相串话。
//
// 这是真实平台验证暴露的缺陷（issue #9 Wave 6）：单用户测试时
// 表现正常，多用户才暴露，属典型「测试覆盖盲区」。
//
// 两者都空时返回哨兵而非空串：空串会让会话键退化成裸 workspace 前缀，
// 让所有此类消息静默撞进同一会话。哨兵使这种畸形输入在审计中可见。
func localSessionID(in *IncomingMessage) string {
	if in.ChatID != "" {
		return in.ChatID
	}
	if in.UserID != "" {
		return in.UserID
	}
	// 既无会话 ID 又无主体 ID——无法安全路由。
	// 用消息 ID 兜底（至少保证不与他人撞车），仍空则用固定哨兵。
	if in.MessageID != "" {
		return "unrouted:" + in.MessageID
	}
	return "unrouted:anonymous"
}

// ResolveRoute 构造会话标识。
//
// base 形如 {channelPrefix}{workspaceId}#{chatId}；话题消息追加话题后缀。
func ResolveRoute(cfg RouteConfig, in *IncomingMessage) RouteTarget {
	if in == nil {
		return RouteTarget{}
	}
	base := ChannelPrefix(in.Platform) + cfg.WorkspaceID + "#" + localSessionID(in)

	// 保守判定：三个条件缺一不可。任一不满足即走普通会话。
	if in.ChatType == ChatGroup && cfg.BindingMode == BindingThreadMap && in.Meta != nil && in.Meta.NativeContextType == nativeContextThread {
		if t := ResolveThread(in.Meta); t != nil {
			return RouteTarget{
				EffectiveJID: base + "#thread:" + t.ContextID + "#root:" + t.RootMessageID,
				AgentID:      deterministicAgentID(base, t.ContextID),
				SourceJID:    base,
			}
		}
	}
	return RouteTarget{EffectiveJID: base, SourceJID: base}
}

// nativeContextThread 是唯一被视为话题的 nativeContextType 值。
const nativeContextThread = "thread"

// deterministicAgentID 由 (会话, 话题) 派生稳定 agent ID。
//
// 必须确定性：同一话题的每条消息都要解析到同一 agent，否则每来一条消息就开一个新 agent。
// 用 SHA-256 而非进程内计数器——后者在重启后漂移，且多实例部署时不收敛。
func deterministicAgentID(base, contextID string) string {
	sum := sha256.Sum256([]byte(base + "\x00" + contextID))
	return "agent-" + hex.EncodeToString(sum[:8])
}

// summarizeTitle 取标题并截断到 titleMaxRunes 个 rune。
func summarizeTitle(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= titleMaxRunes {
		return s
	}
	return string(r[:titleMaxRunes])
}

// firstNonEmpty 返回第一个非空值；全空返回空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
