package cmd

import (
	"fmt"
	"strings"
)

// 内置命令的 Handler（SPEC §4.1）。
//
// **Handler 不负责出站**——它返回回复文本，由调用方统一投递。
// 这样做的收益：Handler 可用纯函数测试，无需 fake Sender。
//
// **Handler 也不做权限判定**——OwnerOnly 属性由 Registry 声明，
// 判定在调用方（它才知道谁是 owner）。本包不 import authz。

// Request 是一次命令调用的上下文（SPEC §3.2）。
//
// 字段是**命令真正需要的最小集**——不预留未使用的字段
// （与项目既有取向一致：不引入无法真实填充的字段）。
type Request struct {
	// Args 是命令名之后的剩余文本。
	Args string
	// SessionID 是当前会话标识（路由后的 EffectiveJID）。
	// /clear 与 /stop 需要它。
	SessionID string
	// SessionGen 是当前会话的代数（/clear 派生新 ID 用）。
	SessionGen int
	// QueuePending 是待处理消息数（/status 展示）。
	QueuePending int
	// WorkspaceID 是工作区标识（/status 展示，便于排障）。
	WorkspaceID string

	// ── /whoami 需要（身份自查）──

	// PrincipalID 是权限判定用的完整主体 ID（可直接粘进 RBAC 配置）。
	//
	// 形态 {workspace}:{platform}:{identityKey}——与 Principal.ID 一致，
	// 因为「配置里该写什么」和「判定时用什么」必须是同一个值，
	// 否则用户会写出看起来对、实际不匹配的配置。
	PrincipalID string
	// IdentitySource 标明身份键来自 union_id 还是 open_id（飞书）。
	//
	// /whoami 据此提示：用 open_id 时身份键**不跨应用**——
	// 多 bot 部署下需在每个应用各绑一次。这个信息用户无法自行推断。
	IdentitySource string
	// OpenID 是平台原生 ID（飞书 open_id，应用维度）。
	// 多 agent 部署下需要知道它——排查「这条消息属于哪个应用」时要用。
	OpenID string
	// Agent 是本次消息被分流到的 agent 名（多 agent 部署）。
	// 空串表示单 agent。
	Agent string
}

// Deps 是 Handler 需要的外部能力。
//
// 用接口而非具体类型：让 Handler 可独立测试，
// 且**不把 server 包的类型引入 cmd 包**（避免循环依赖）。
type Deps struct {
	// ClearSession 让当前会话进入新的一代（SPEC §11.1.2：换 sessionID）。
	//
	// 返回新会话的代数，供回复文本展示（便于排障）。
	ClearSession func(sessionID string) int
	// CancelRun 取消当前会话正在执行的 run（SPEC §5.4）。
	//
	// 返回是否真的取消了一个 run——false 表示当时没有进行中的生成。
	CancelRun func(sessionID string) bool
}

// RegisterBuiltins 把内置命令注册到注册表（SPEC §4.1）。
//
// v1 的 4 条（/help /status /clear /stop）加上 /whoami（身份自查）。
// /owner_mention 与 /release_owner 移出 v1（需 owner 持久化，
// 见 SPEC §11.1.1）。不注册 = 用户发它们时走正常消息路径，
// 不会报错也不会误触发。
func RegisterBuiltins(r *Registry, deps Deps) {
	r.MustRegister(Command{
		Name:  "help",
		Usage: "/help",
		Desc:  "显示本帮助",
		Handler: func(req Request) (string, error) {
			// /help 忽略 args（SPEC §5.5）。
			return r.HelpText(), nil
		},
	})

	r.MustRegister(Command{
		Name:  "whoami",
		Usage: "/whoami",
		Desc:  "查看自己的身份（权限配置用）",
		// NoPermission：解开「配权限需要 ID、查 ID 需要权限」的死锁。
		// 只回显调用者本人的主体 ID，不含他人数据、不改状态。
		NoPermission: true,
		Handler: func(req Request) (string, error) {
			return whoamiText(req), nil
		},
	})

	r.MustRegister(Command{
		Name:  "status",
		Usage: "/status",
		Desc:  "查看会话状态",
		Handler: func(req Request) (string, error) {
			var sb strings.Builder
			sb.WriteString("会话状态：\n")
			fmt.Fprintf(&sb, "  会话 ID：%s\n", req.SessionID)
			if req.SessionGen > 0 {
				fmt.Fprintf(&sb, "  代数：%d（/clear 后递增）\n", req.SessionGen)
			}
			fmt.Fprintf(&sb, "  待处理消息：%d\n", req.QueuePending)
			if req.WorkspaceID != "" {
				fmt.Fprintf(&sb, "  工作区：%s", req.WorkspaceID)
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	})

	r.MustRegister(Command{
		Name:      "clear",
		Usage:     "/clear",
		Desc:      "开始新会话（历史隔离）",
		OwnerOnly: true,
		Handler: func(req Request) (string, error) {
			if deps.ClearSession == nil {
				return "", fmt.Errorf("cmd: /clear 不可用（未装配 ClearSession）")
			}
			gen := deps.ClearSession(req.SessionID)
			return fmt.Sprintf(
				"已开始新会话（代数 %d）。之前的对话历史不再参与本次对话。", gen), nil
		},
	})

	r.MustRegister(Command{
		Name:      "stop",
		Usage:     "/stop",
		Desc:      "中断当前生成",
		OwnerOnly: true,
		Handler: func(req Request) (string, error) {
			if deps.CancelRun == nil {
				return "", fmt.Errorf("cmd: /stop 不可用（未装配 CancelRun）")
			}
			if !deps.CancelRun(req.SessionID) {
				// 没有进行中的 run 不是错误——是正常状态（SPEC §5.4 边界）。
				return "当前没有正在生成的回答。", nil
			}
			return "已中断当前生成。", nil
		},
	})
}

// whoamiText 生成身份自查文本（/whoami）。
//
// 为什么需要这条命令：权限配置要求把**主体 ID** 写进 TAIJI_RBAC /
// TAIJI_FEISHU_OWNERS，但用户无从知道那个 ID 是什么——它由平台元数据
// 加渠道前缀拼成，无法自行推导。没有自查手段时只能读日志猜，
// 而日志里是**脱敏摘要**（见 Principal.Redacted），不是可配置的原值。
//
// 输出的 ID 与实际判定用的值**同源**（都是 Principal.ID），
// 避免用户写出看起来对、实际不匹配的配置。
func whoamiText(req Request) string {
	var sb strings.Builder
	sb.WriteString("你的身份：\n")

	id := req.PrincipalID
	if id == "" {
		// 无身份：不编造。明确说明后果，而不是给一个空行让人困惑。
		sb.WriteString("  主体 ID：<无法确定——本条消息未携带平台身份>\n")
		sb.WriteString("  后果：按用户判定的权限（RBAC / owner_only）对你一律不生效。\n")
		return strings.TrimRight(sb.String(), "\n")
	}
	fmt.Fprintf(&sb, "  主体 ID：%s\n", id)

	// 配置位置提示——用户拿到 ID 后要知道写到哪。
	sb.WriteString("  配置位置：TAIJI_RBAC 的 user:<上面这串>=<角色名>\n")

	if req.Agent != "" {
		fmt.Fprintf(&sb, "  Agent：%s\n", req.Agent)
	}

	// 身份键来源：这决定「配置一次是否够用」。
	switch req.IdentitySource {
	case "union_id":
		sb.WriteString("  身份键来源：union_id（跨应用稳定）\n")
		sb.WriteString("    同一个你在所有 bot 下都是上面这个 ID——配置一次即可。\n")
	case "open_id":
		sb.WriteString("  身份键来源：open_id（**应用维度，不跨应用**）\n")
		sb.WriteString("    ⚠ 同一个你在不同 bot 下 ID 不同——每个 bot 都要单独配一次。\n")
		sb.WriteString("    取到 union_id 可免除此重复（需应用具备通讯录权限）。\n")
	}

	// open_id 单独列出：多 agent 部署下排查「哪个应用」时需要它。
	if req.OpenID != "" {
		fmt.Fprintf(&sb, "  平台 open_id：%s（本应用下）\n", req.OpenID)
	}
	return strings.TrimRight(sb.String(), "\n")
}
