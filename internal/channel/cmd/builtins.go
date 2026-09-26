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

// RegisterBuiltins 把 4 条内置命令注册到注册表（SPEC §4.1）。
//
// **只注册 v1 的 4 条**——/owner_mention 与 /release_owner 移出 v1
// （需 owner 持久化，见 SPEC §11.1.1）。不注册 = 用户发它们时
// 走正常消息路径，不会报错也不会误触发。
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
