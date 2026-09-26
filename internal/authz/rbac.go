package authz

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RBAC 权限源（Wave 2——决策三）。
//
// 与 StaticPermissions（方案 A）的关系：
//
//	StaticPermissions   User ──N×M──▶ Permission      （RBAC0 退化：直连）
//	RBACPermissions     User ──N×R──▶ Role ──R×M──▶ Permission
//
// 补上 Role 层后，配置量从 N×M 降到 N+R×M——这是 RBAC 的唯一实质收益
// （用户规模上来后才成立；个位数用户时 RBAC 是过度设计）。
//
// 支持 RBAC1（角色继承）：子角色自动获得父角色的权限。
// 不做 RBAC2（互斥角色/基数约束）——对当前规模是重维护税。
//
// 权限点是**裸工具名**（v1），支持 "*" 与前缀通配——复用 matchToolPattern，
// 与 StaticPermissions 语义一致。资源级动作（"ws.modify" 等）是 v2，
// 前缀与裸工具名不会冲突（工具名不含 "."）。
type RBACConfig struct {
	// Roles 是角色 → 权限点列表。
	//	{"operator": {"mockmcp_echo", "infraverse_*"}}
	Roles map[string][]string
	// UserRoles 是用户（Principal.ID）→ 角色名列表。
	//	{"ws1:feishu:ou_alice": {"operator"}}
	UserRoles map[string][]string
	// RoleParents 是角色 → 父角色列表（RBAC1 继承）。
	//	{"operator": {"viewer"}}  // operator 继承 viewer 的权限
	RoleParents map[string][]string
}

// RBACPermissions 是装配后的 RBAC 权限源。
//
// 构造时把「用户 → 角色 → 展开后的权限点」预计算好——判定是纯内存查表，
// 无需每次走继承链。代价是配置变更需重建（v1 静态配置，可接受）。
type RBACPermissions struct {
	// byUser 是用户 → 展开后的权限点列表（含继承）。
	byUser map[string][]string
	// roleCount / userCount 供启动期日志。
	roleCount int
	userCount int
}

// NewRBACPermissions 构造 RBAC 权限源。
//
// nil / 空配置 → 拒绝一切（安全基线，与 StaticPermissions 一致）。
func NewRBACPermissions(cfg RBACConfig) *RBACPermissions {
	byUser := make(map[string][]string, len(cfg.UserRoles))

	for user, roles := range cfg.UserRoles {
		user = strings.TrimSpace(user)
		if user == "" {
			continue
		}
		perms := collectPermissions(roles, cfg.Roles, cfg.RoleParents)
		if len(perms) > 0 {
			byUser[user] = perms
		}
	}

	return &RBACPermissions{
		byUser:    byUser,
		roleCount: len(cfg.Roles),
		userCount: len(byUser),
	}
}

// collectPermissions 展开角色列表的权限（含继承），去重后返回。
//
// 继承用 BFS + visited 防环——配置里出现环（a→b→a）时必须终止，
// 否则判定会死循环。visited 同时完成去重。
func collectPermissions(roles []string, rolePerms, roleParents map[string][]string) []string {
	seen := make(map[string]bool)   // 已访问的角色（防环）
	perms := make(map[string]bool)  // 已收集的权限点（去重）
	var order []string              // 保持稳定顺序（便于测试与日志）

	queue := make([]string, 0, len(roles))
	for _, r := range roles {
		if r = strings.TrimSpace(r); r != "" {
			queue = append(queue, r)
		}
	}

	for len(queue) > 0 {
		role := queue[0]
		queue = queue[1:]
		if seen[role] {
			continue // 已展开（含环的回边）
		}
		seen[role] = true

		for _, p := range rolePerms[role] {
			if p = strings.TrimSpace(p); p != "" && !perms[p] {
				perms[p] = true
				order = append(order, p)
			}
		}
		// 父角色入队（继承）。
		for _, parent := range roleParents[role] {
			if parent = strings.TrimSpace(parent); parent != "" && !seen[parent] {
				queue = append(queue, parent)
			}
		}
	}
	return order
}

// Validate 校验配置的一致性：所有被引用的角色名必须**已定义**。
//
// 动机（实测发现的静默失效）：若 user 绑定到一个不存在的角色（拼写错误），
// 该用户的权限为空 → 所有工具调用被拒，而启动无提示。parent 引用不存在的
// 角色同理（死引用）。这两类都是「配了却不生效」的静默失效。
//
// 「已定义」的两个来源：
//   - Roles 的键（有直接权限的角色）
//   - RoleParents 的键（分组角色，可能无直接权限）
//
// **RoleParents 的值不构成定义**——一个「只被继承、自身无权限定义」的角色
// 在 v1 静态配置下没有意义（不带来权限，只让继承链多一跳），且极可能是
// 拼写错误。把它当已定义会放过这类错误（实测暴露的漏洞）。
//
// 全部错误一次性列出（便于一次修完，而非逐个试）。
func (c RBACConfig) Validate() error {
	defined := make(map[string]bool)
	for name := range c.Roles {
		defined[name] = true
	}
	for name := range c.RoleParents {
		defined[name] = true
	}

	var problems []string
	for user, roles := range c.UserRoles {
		for _, r := range roles {
			if r = strings.TrimSpace(r); r != "" && !defined[r] {
				problems = append(problems,
					fmt.Sprintf("user %q 引用了未定义的角色 %q", user, r))
			}
		}
	}
	for child, parents := range c.RoleParents {
		for _, p := range parents {
			if p = strings.TrimSpace(p); p != "" && !defined[p] {
				problems = append(problems,
					fmt.Sprintf("parent %q 引用了未定义的角色 %q", child, p))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems) // 稳定输出，便于测试与阅读
	return fmt.Errorf("RBAC 配置有 %d 处未定义角色引用：\n  - %s",
		len(problems), strings.Join(problems, "\n  - "))
}

// Allowed 实现 PermissionSource。
//
// 空主体 / 空动作 → 确定的拒绝（false, nil）——不是查询失败。
// 未绑定角色的用户 → 拒绝（fail-closed）。
//
// v1 只看 Action（工具名）；Resource 透传但不参与判定（资源级是 v2）。
func (p *RBACPermissions) Allowed(_ context.Context, req AccessRequest) (bool, error) {
	if p == nil || !req.Principal.Valid() || req.Action == "" {
		return false, nil
	}
	perms, ok := p.byUser[req.Principal.ID]
	if !ok {
		return false, nil // 未绑定角色：拒绝
	}
	for _, pat := range perms {
		if matchToolPattern(pat, req.Action) {
			return true, nil
		}
	}
	return false, nil
}

// RoleCount 返回角色数（供启动期日志）。
func (p *RBACPermissions) RoleCount() int {
	if p == nil {
		return 0
	}
	return p.roleCount
}

// UserCount 返回已绑定角色的用户数（供启动期日志）。
func (p *RBACPermissions) UserCount() int {
	if p == nil {
		return 0
	}
	return p.userCount
}
