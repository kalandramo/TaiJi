// Package agentreg 维护「飞书应用 ID → agent 名」的分流映射。
//
// 依据：多 agent 部署下每个 agent 绑定独立的飞书应用（ak/sk），
// 入站消息需按**接收该消息的应用**（事件头的 Header.AppID）分流到
// 对应 agent。这是形态 C（Bot 即 agent）的核心判定。
//
// 与 channel/router.go 的 AgentID 的关系：后者由 (会话, 话题) 派生，
// 用于「同一群内不同话题各自独立 agent」；本包按**应用**分流，
// 两者语义不同。当前只启用本包（AppID 分流），router.AgentID 保留不动。
package agentreg

import (
	"errors"
	"fmt"
	"sort"
)

// ErrUnknownApp 表示消息来自未注册的飞书应用。
//
// **必须 fail-closed**：若未知 AppID 静默落到默认 agent，一个新 bot
// 配错就会把消息喂给错误 agent，且表现为「功能正常但回答不对」——
// 这类静默错配最难排查（与项目既有取向一致：gate.go 对
// bot_open_id 缺失也是 fail-closed）。
var ErrUnknownApp = errors.New("agentreg: unknown app_id")

// Registry 是 AppID → agent 名的映射。
//
// 并发安全：启动期构造后只读，故不加锁（与 cmd.Registry 同约定）。
type Registry struct {
	byAppID map[string]string
	// order 保持构造顺序，供日志稳定输出。
	order []string
}

// Entry 是一条映射（注册表构造用）。
type Entry struct {
	AppID string
	Agent string
}

// New 构造注册表。
//
// 空列表返回 nil（表示未启用多 agent 分流）——调用方据此走单 agent 路径，
// 与既有行为完全一致。
func New(entries []Entry) (*Registry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	byAppID := make(map[string]string, len(entries))
	order := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.AppID == "" {
			return nil, fmt.Errorf("agentreg: entry for agent %q has empty app_id", e.Agent)
		}
		if e.Agent == "" {
			return nil, fmt.Errorf("agentreg: app_id %q has empty agent name", e.AppID)
		}
		if seen[e.AppID] {
			// 同一 AppID 映射到两个 agent ⇒ 分流结果不确定。
			return nil, fmt.Errorf(
				"agentreg: app_id %q 重复——同一应用的消息无法确定归属", e.AppID)
		}
		seen[e.AppID] = true
		byAppID[e.AppID] = e.Agent
		order = append(order, e.AppID)
	}
	return &Registry{byAppID: byAppID, order: order}, nil
}

// Resolve 返回该 AppID 对应的 agent 名。
//
// 未注册的 AppID 返回 ErrUnknownApp（fail-closed，见该变量的说明）。
func (r *Registry) Resolve(appID string) (string, error) {
	if r == nil {
		// nil 注册表 = 未启用多 agent。返回空 agent 名，调用方走单 agent。
		return "", nil
	}
	agent, ok := r.byAppID[appID]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownApp, appID)
	}
	return agent, nil
}

// Agents 返回全部 agent 名（排序，供日志与测试）。
func (r *Registry) Agents() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]bool, len(r.byAppID))
	var out []string
	for _, a := range r.byAppID {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// Len 返回映射条目数。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.order)
}
