// Package authz 的用户级权限（方案 A：静态配置）。
//
// 与 toolpolicy.go 的分工——两者是**串联的两个维度**，不是替代关系：
//
//	维度        插件              判定依据              变更频率
//	部署级      approval（既有）   启动期白名单          低（改配置重启）
//	用户级      PrincipalPolicy    本文件的权限表        高（可热更新）
//
// 执行顺序：先过部署白名单（工具是否在本部署启用），
// 再过用户权限（该用户能否用这个工具）。任一拒绝即不执行。
//
// 为什么分开：混在一起会让"改部署配置"和"改用户权限"互相干扰；
// 且 approval 是上游 SDK 实现，改它意味着 fork。
package authz

import (
	"context"
	"strings"
)

// AccessRequest 是一次权限查询的完整上下文（issue #6 决策二）。
//
// 从 (principal, toolName) 扩为三元组，是为了让「资源级」判定无需二次
// 改接口——v1 填 Action=工具名、Resource=工作区 ID（由 ctx 注入）。
//
// 三者正交：
//   - Principal 管「谁」
//   - Action    管「做什么」（v1：工具名；v2：动作如 "ws.modify"）
//   - Resource  管「对什么」（v1：工作区 ID；v2：细化到路径/参数摘要）
type AccessRequest struct {
	// Principal 是发起请求的主体。
	Principal Principal
	// Action 是动作标识。v1 即工具名（如 "mockmcp_echo"）。
	Action string
	// Resource 是资源标识。v1 为工作区 ID（可能为空——非渠道来源）。
	Resource string
}

// PermissionSource 回答"某主体能否执行某动作"。
//
// 抽象出来的理由：数据源形态可变（静态配置 / RBAC / 外部权限中心 / 混合），
// 而消费方（插件）不关心数据从哪来。换数据源只需换实现。
type PermissionSource interface {
	// Allowed 判断该请求是否被允许。
	//
	// 返回 error 表示**无法判定**（数据源不可达等），与 (false, nil)
	// 语义不同：
	//   - (false, nil) —— 查了，不允许
	//   - (false, err) —— 查不了
	// 两者都必须拒绝，但日志要区分——否则数据源故障会被误读为权限收紧。
	Allowed(ctx context.Context, req AccessRequest) (bool, error)
}

// StaticPermissions 是静态权限表（方案 A）。
//
// 形态：主体 ID → 工具名模式列表。
//
//	{
//	  "ws1:feishu:ou_alice": {"mockmcp_echo", "infraverse_*"},
//	  "ws1:feishu:ou_admin": {"*"},
//	}
//
// 匹配规则：
//   - 精确匹配优先
//   - 支持前缀通配 "srv_*"（匹配 srv_ 开头的任意工具名）
//   - "*" 匹配一切（显式授权）
//
// 安全基线：未列出的主体、未列出的工具、空主体、nil 表 —— 一律拒绝。
type StaticPermissions struct {
	byPrincipal map[string][]string
}

// NewStaticPermissions 用权限表构造。nil 或空表表示拒绝一切。
//
// 构造时做规范化（去空白、丢弃空条目），避免运行时反复处理。
func NewStaticPermissions(table map[string][]string) *StaticPermissions {
	if len(table) == 0 {
		return &StaticPermissions{byPrincipal: map[string][]string{}}
	}
	out := make(map[string][]string, len(table))
	for k, patterns := range table {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		cleaned := make([]string, 0, len(patterns))
		for _, p := range patterns {
			if p = strings.TrimSpace(p); p != "" {
				cleaned = append(cleaned, p)
			}
		}
		if len(cleaned) > 0 {
			out[key] = cleaned
		}
	}
	return &StaticPermissions{byPrincipal: out}
}

// Allowed 实现 PermissionSource。
//
// 空主体是**确定的拒绝**（返回 false, nil 而非 error）——它不是查询失败，
// 而是"没有身份可供判定"。调用方无需特殊处理。
//
// v1 只看 Action（工具名）：Resource 已透传但不参与判定（资源级是 v2）。
func (s *StaticPermissions) Allowed(_ context.Context, req AccessRequest) (bool, error) {
	if !req.Principal.Valid() || req.Action == "" {
		return false, nil
	}
	patterns, ok := s.byPrincipal[req.Principal.ID]
	if !ok {
		return false, nil // 未列出的主体：拒绝
	}
	for _, pat := range patterns {
		if matchToolPattern(pat, req.Action) {
			return true, nil
		}
	}
	return false, nil
}

// matchToolPattern 判断工具名是否匹配模式。
//
// 支持：
//   - "*"        匹配一切
//   - "prefix_*" 前缀匹配
//   - 其他       精确匹配（大小写敏感，与工具名校验一致）
func matchToolPattern(pattern, toolName string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(toolName, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == toolName
}

// PrincipalCount 返回表中有权限条目的主体数（供启动期日志）。
func (s *StaticPermissions) PrincipalCount() int {
	if s == nil {
		return 0
	}
	return len(s.byPrincipal)
}
