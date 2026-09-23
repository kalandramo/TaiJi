// 会话串行化：保证同一会话的消息串行处理，不并发污染同一份 session log。
//
// 依据设计文档 §4.5（docs/03-原型设计文档.md:660，NFR-8）。
//
// 为什么原型就要做：runner.Run 每次返回独立 goroutine，但未约束同一会话的并发。
// 用户连发两条消息 → 两个 run 并发 → session log 交错 → 回答错乱。
//
// 核心设计（NFR-8.2）：匹配依据是**派生键**而非 JID。同一 workspace 下的多个
// 会话共享串行化域——否则虚拟 JID（话题后缀）会让本该串行的消息并发起来。
//
// 原型简化（§4.5 末段）：不做 NFR-8.3 的「容量准入按执行模式分叉」（原型只有
// 进程内模式，串行化键即唯一准入边界），不做 NFR-8.5 的游标持久化（队列在内存，
// 重启丢待处理消息可接受）。
package concurrency

import (
	"context"
	"strings"
	"sync"
)

// Serializer 是进程内串行化器。
//
// 零值不可用——必须经 NewSerializer 构造。
type Serializer struct {
	mu   sync.Mutex
	busy map[string]struct{}
	// waiters 按派生键登记排队者。每个等待者持有一个 buffered channel，
	// release 时唤醒队首。
	waiters map[string][]chan struct{}
}

// NewSerializer 构造串行化器。
func NewSerializer() *Serializer {
	return &Serializer{
		busy:    make(map[string]struct{}),
		waiters: make(map[string][]chan struct{}),
	}
}

// DeriveKey 从会话标识派生串行化键（纯字符串，零 IO、无 error）。
//
// 取 '#' 之前的部分，使同一 workspace 的多个会话共享串行化域（NFR-8.2）。
// 例：feishu:ws1#chatA 与 feishu:ws1#chatB → 同为 feishu:ws1。
//
// 为什么不用 JID 本身：话题路由会为同一 workspace 生成多个虚拟 JID
// （#thread:...#root:...）。以 JID 为键则这些会话互不阻塞，同一份 session
// log 仍可能被并发写入——恰恰违背串行化的初衷。
//
// 代价（有意为之）：串行粒度是 workspace 而非会话，同 workspace 内不同会话
// 也互斥。这是「绝不交错」优先于「最大吞吐」的取舍（v1.4 NFR-8.2）。
func DeriveKey(jid string) string {
	if i := strings.IndexByte(jid, '#'); i >= 0 {
		return jid[:i]
	}
	return jid
}

// Acquire 尝试占用串行化域。已占用返回 (nil, false)，调用方自行决定重试或排队。
//
// 非阻塞语义：这是「乐观尝试」，适合调用方已有自己的排队机制时使用。
// 需要阻塞排队请用 AcquireBlocking。
func (s *Serializer) Acquire(jid string) (release func(), ok bool) {
	key := DeriveKey(jid)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.busy[key]; busy {
		return nil, false
	}
	s.busy[key] = struct{}{}
	return s.makeRelease(key), true
}

// AcquireBlocking 占用串行化域，已占用则排队等待。
//
// 满足 AC-4：release 被调用后，等待中的消息能获取到域并继续处理。
// ctx 取消时返回 ctx.Err()——排队必须可取消，否则关闭流程会挂住。
//
// 公平性：FIFO。release 唤醒队首等待者，避免饥饿。
func (s *Serializer) AcquireBlocking(ctx context.Context, jid string) (release func(), err error) {
	key := DeriveKey(jid)

	for {
		s.mu.Lock()
		if _, busy := s.busy[key]; !busy {
			s.busy[key] = struct{}{}
			s.mu.Unlock()
			return s.makeRelease(key), nil
		}
		// 登记为等待者。buffered(1) 使 release 的唤醒不会阻塞。
		ch := make(chan struct{}, 1)
		s.waiters[key] = append(s.waiters[key], ch)
		s.mu.Unlock()

		select {
		case <-ch:
			// 被唤醒：重试占用。此时域可能已被其他竞争者抢走（唤醒后到重试前
			// 存在窗口），故回到循环再判断，而非直接假定成功。
			continue
		case <-ctx.Done():
			s.removeWaiter(key, ch)
			return nil, ctx.Err()
		}
	}
}

// makeRelease 返回释放函数。幂等：重复调用不会误删他人的占用。
func (s *Serializer) makeRelease(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.busy, key)
			// 唤醒队首等待者（若有）。
			if q := s.waiters[key]; len(q) > 0 {
				head := q[0]
				s.waiters[key] = q[1:]
				if len(s.waiters[key]) == 0 {
					delete(s.waiters, key)
				}
				s.mu.Unlock()
				// 非阻塞发送：head 是 buffered(1) 且只被唤醒一次。
				select {
				case head <- struct{}{}:
				default:
				}
				return
			}
			s.mu.Unlock()
		})
	}
}

// removeWaiter 在取消时摘除排队登记，避免唤醒一个已放弃的等待者。
func (s *Serializer) removeWaiter(key string, ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.waiters[key]
	for i, c := range q {
		if c == ch {
			s.waiters[key] = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(s.waiters[key]) == 0 {
		delete(s.waiters, key)
	}
}
