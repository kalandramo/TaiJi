package server

import (
	"context"
	"sync"
)

// 会话级取消注册表（SPEC §5.4）——/stop 的基础设施。
//
// **为什么需要它**（实测发现的缺口）：dispatch.go 的 worker 每条消息用
// `context.Background()`，它**不可取消**。生产代码全仓无 WithCancel/WithTimeout。
//
// 后果：/stop 即使实现了命令识别，也无法中断正在生成的 run——
// 用户会看到「已停止」但生成仍在继续（假功能）。
//
// 本文件补上这个基础设施：让每个 run 的 ctx 可被外部取消。

// cancelEntry 是一次登记。
//
// 带 gen（代际号）而非直接存 CancelFunc：cleanup 需要判断
// 「我的条目是否已被后续 Register 替换」。Go 无法比较函数值，
// 故用代际号做身份标识——比函数指针比较可靠。
type cancelEntry struct {
	cancel context.CancelFunc
	gen    uint64
}

// CancelRegistry 管理 per-session 的取消函数。
//
// 并发安全：/stop 来自另一个 goroutine（另一个 worker 处理的消息），
// 与正在执行的 run 并发。故必须加锁。
//
// 生命周期：Register 返回的 cleanup 必须被 defer 调用——否则条目泄漏
// （每个会话累积一个永不释放的 CancelFunc）。
type CancelRegistry struct {
	mu      sync.Mutex
	cancels map[string]cancelEntry
	nextGen uint64
}

// NewCancelRegistry 构造空注册表。
func NewCancelRegistry() *CancelRegistry {
	return &CancelRegistry{cancels: make(map[string]cancelEntry)}
}

// Register 为 sessionID 登记一个可取消的 ctx。
//
// 返回的 ctx 供执行层使用；返回的 cleanup **必须** defer 调用。
//
// 若同一 sessionID 已有登记（理论上不该发生——串行化保证同会话不并发），
// 旧的会被取消并替换。这是**保守选择**：宁可取消一个不该取消的 run，
// 也不留下两个竞争同一 session 的 run。
//
// cleanup 幂等：重复调用安全（once 保护）。
func (r *CancelRegistry) Register(parent context.Context, sessionID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)

	r.mu.Lock()
	r.nextGen++
	gen := r.nextGen
	if prev, exists := r.cancels[sessionID]; exists {
		// 保守：取消旧的（见函数注释）。
		prev.cancel()
	}
	r.cancels[sessionID] = cancelEntry{cancel: cancel, gen: gen}
	r.mu.Unlock()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			r.mu.Lock()
			// 只删**自己这一代**的条目——若已被后续 Register 替换
			// （gen 不同），不误删新的。
			if cur, ok := r.cancels[sessionID]; ok && cur.gen == gen {
				delete(r.cancels, sessionID)
			}
			r.mu.Unlock()
			cancel() // 释放 ctx 资源（cancel 自身幂等）
		})
	}
	return ctx, cleanup
}

// Cancel 取消 sessionID 对应的 run。
//
// 返回是否真的取消了一个 run——false 表示当时没有进行中的生成。
// 调用方（/stop 的 Handler）据此给出不同回复（SPEC §5.4 边界）。
//
// **不删除条目**：由 cleanup 负责（它知道自己的代际）。
// 若在此处删除，可能与正在执行的 cleanup 竞争。
func (r *CancelRegistry) Cancel(sessionID string) bool {
	r.mu.Lock()
	entry, ok := r.cancels[sessionID]
	r.mu.Unlock()

	if !ok {
		return false
	}
	entry.cancel()
	return true
}

// Pending 返回当前登记的会话数（观测与测试用）。
func (r *CancelRegistry) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cancels)
}
