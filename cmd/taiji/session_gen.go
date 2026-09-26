package main

import "sync"

// 会话代数计数（SPEC §11.1.2 决策：/clear 用「换 sessionID」）。
//
// **职责边界（重要）**：本类型**只做代数计数**——
// 「当前是第几代」「递增到下一代」。
//
// sessionID 的**作用域化**（把代数后缀叠上去）由 server.Pipeline 做，
// 因为它才知道路由产生的 EffectiveJID（本包看不到）。
//
// 若把两者混在本包，会出现「递增了代数但没人用」的空操作——
// 用户看到「已开始新会话」但历史仍参与后续对话（假功能）。
//
// 为什么不用「清空 session service 的历史」：那需要访问 runner 的内部
// session service（当前无暴露接口，见 internal/chat/execute.go 的
// assembly 未导出它）。换 sessionID 只需在路由结果上叠后缀。
//
// 代价（明说）：旧 session 的数据**不删除**（仍在 session service 内存里），
// 只是不再被引用。对原型可接受（进程重启即释放）。

// sessionGenerations 管理会话的代数（并发安全）。
//
// 并发安全理由：/clear 来自一个 worker，而读取代数发生在另一个
// worker 处理的消息里——两者并发。
type sessionGenerations struct {
	mu   sync.Mutex
	gens map[string]int
}

func newSessionGenerations() *sessionGenerations {
	return &sessionGenerations{gens: make(map[string]int)}
}

// Current 返回会话的当前代数（0 表示未 /clear 过）。
func (g *sessionGenerations) Current(sessionID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gens[sessionID]
}

// Next 让会话进入新的一代，返回新代数。
//
// 代数从 1 开始（0 保留给「未 clear 过」的初始状态）——
// 使「有后缀」与「无后缀」语义清晰。
func (g *sessionGenerations) Next(sessionID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gens[sessionID]++
	return g.gens[sessionID]
}
