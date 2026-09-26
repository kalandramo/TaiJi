package main

import (
	"fmt"
	"os"
	"strings"
)

// serve 路径的权限配置一致性校验（issue #6 缺口 3）。
//
// 背景：工具策略是「默认拒绝 + 白名单放行」（部署级），但**用户级**权限
// 表缺失时，原实现是「不做用户级判定」——即任何能触发 bot 的用户都能用
// 所有已放行工具。这在 CLI 单用户场景合理，在 serve 多渠道场景是
// **安全边界消失**。
//
// 本校验把「遗漏」与「有意放开」区分开：遗漏 → 拒绝启动；
// 有意放开 → 需显式设 TAIJI_ALLOW_ALL_USERS=1（留下决策痕迹）。
//
// 与缺口 1 的同构：都是「声明与执行不一致」。缺口 1 是上下文级降权未接线，
// 本缺口是用户级权限的默认值在两种场景下语义相反。
//
// 2026-09-26 更新：用户级权限唯一来源已是 TAIJI_RBAC
// （TAIJI_USER_PERMISSIONS 退役），错误信息随之改指 RBAC。

// envAllowAllUsers 是显式放开用户级管控的开关。
//
// 为什么需要它：单用户部署（或纯只读工具部署）确实不需要按用户管控，
// 但要让它成为**显式决策**而非遗漏——否则「忘了配」与「有意放开」
// 在系统里无法区分。
const envAllowAllUsers = "TAIJI_ALLOW_ALL_USERS"

// servePermInput 是校验的输入。
type servePermInput struct {
	// HasTools 表示本次装配是否挂了工具（无工具则无安全边界可失）。
	HasTools bool
	// HasPermissions 表示是否配了用户级权限表。
	HasPermissions bool
	// AllowAllUsers 表示是否显式声明放开用户级管控。
	AllowAllUsers bool
}

// validateServePermissions 校验 serve 路径的权限配置一致性。
//
// 规则：有工具可调用时，必须有明确的用户级权限决策——
// 配了权限表，或显式放开。二者皆无 → 拒绝（fail-closed）。
//
// 无工具时放行：没有可调用的工具，就没有「越权使用工具」的风险，
// 要求配置权限表是多余的仪式。
func validateServePermissions(in servePermInput) error {
	if !in.HasTools {
		return nil // 无工具可调用，无边界可失
	}
	if in.HasPermissions {
		return nil // 按用户管控已生效
	}
	if in.AllowAllUsers {
		return nil // 显式放开（有意决策，非遗漏）
	}
	return fmt.Errorf(
		"权限配置缺失：已装配工具，但未配置用户级权限——"+
			"任何能触发 bot 的用户都可使用已放行的工具。"+
			"请二选一：(1) 设 TAIJI_RBAC 按用户/角色管控；"+
			"(2) 若确为单用户/只读场景，显式设 %s=1 声明放开",
		envAllowAllUsers)
}

// allowAllUsersFromEnv 读显式放开开关。
//
// 只认 "1"/"true"（大小写不敏感）——避免 "0"/"false" 被误判为真。
func allowAllUsersFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envAllowAllUsers))) {
	case "1", "true":
		return true
	default:
		return false
	}
}
