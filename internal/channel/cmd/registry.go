// Package cmd 实现 IM 命令的识别、注册与分发。
//
// 依据 docs/09-命令系统SPEC.md。
//
// 分层定位：本包**只做命令本身**——解析、注册表、Handler。
// 权限判定与出站由调用方（server 包）负责，因为那是编排职责。
// 本包**不 import authz**，保持「命令逻辑」与「权限机制」解耦
// （SPEC §9.4 AC-D1：PermissionSource 是唯一替换接缝）。
//
// 为什么不用 cobra（SPEC §1.3）：cobra 是 CLI 参数解析器——它处理
// os.Args、--flag、子命令树。IM 命令是**纯文本**（"/help"），
// 没有 flag、没有嵌套子命令。引入 cobra 是 7 个模块依赖换 90%
// 用不上的能力，与本项目「casbin 不引入」的取舍一致。
package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// maxCommandNameLen 是命令名最大长度。
//
// 约束理由：防止超长文本被误判为命令（SPEC §5.2）。
const maxCommandNameLen = 32

// maxContentLen 是触发命令判定的消息长度上限。
//
// 超过此长度时**不做命令判定**——`/` 开头的超长文本更可能是用户
// 粘贴的路径或代码，不是命令（SPEC §5.2）。
const maxContentLen = 4096

// ErrInvalidName 表示命令名不符合规范。
var ErrInvalidName = errors.New("cmd: invalid command name")

// ErrDuplicate 表示命令名重复注册。
var ErrDuplicate = errors.New("cmd: duplicate command name")

// ErrEmptyHandler 表示未提供 Handler。
var ErrEmptyHandler = errors.New("cmd: handler is required")

// Command 是一条命令的定义（SPEC §3.2）。
type Command struct {
	// Name 是命令名（不含 "/"），小写，形如 [a-z_]{1,32}。
	Name string
	// Usage 是用法示例，如 "/stop"。
	Usage string
	// Desc 是一句话说明（进 /help 输出）。
	Desc string
	// OwnerOnly 标记「仅工作区 owner 可执行」。
	//
	// 注意：OwnerOnly 与 RBAC 权限点是**两个正交维度**——
	// owner 是「资源属于谁」，RBAC 是「主体能做什么」。两者都要过。
	// 本包只**声明**该属性，判定由调用方做（它才知道谁是 owner）。
	OwnerOnly bool
	// Handler 执行命令。
	//
	// 返回的 string 是给用户的回复文本；error 表示执行失败。
	// Handler **不负责出站**——由调用方统一投递（便于测试与审计）。
	//
	// 参数是 Request（含 Args/SessionID 等上下文）而非裸 args 字符串：
	// /status 需要会话信息、/clear 与 /stop 需要 sessionID——
	// 裸字符串无法承载这些。见 builtins.go 的 Request 定义。
	Handler func(req Request) (string, error)
}

// Registry 是命令注册表（SPEC §3.2）。
//
// 并发安全：注册在启动期完成，之后只读。故不加锁——
// 但**要求**调用方不在运行期注册（Register 在装配后不再调用）。
type Registry struct {
	// order 保持注册顺序——/help 输出顺序稳定，便于测试与阅读。
	order []string
	// byName 是名字 → 命令。
	byName map[string]Command
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Command)}
}

// Register 注册一条命令。
//
// 校验失败即返回 error（**不 panic**）——让装配方决定如何处理。
// 但 SPEC §5.2 要求「启动期暴露」，故调用方（main）应把它当致命错误。
//
// 校验项：
//   - Name 符合 ^[a-z_]{1,32}$
//   - Name 未重复
//   - Handler 非 nil
func (r *Registry) Register(c Command) error {
	if err := validateName(c.Name); err != nil {
		return err
	}
	if c.Handler == nil {
		return fmt.Errorf("%w (name=%s)", ErrEmptyHandler, c.Name)
	}
	if _, exists := r.byName[c.Name]; exists {
		return fmt.Errorf("%w (name=%s)", ErrDuplicate, c.Name)
	}
	r.byName[c.Name] = c
	r.order = append(r.order, c.Name)
	return nil
}

// MustRegister 注册并在失败时 panic。
//
// 用于启动期装配——配置错误应立即暴露，而非延迟到用户发命令时。
// SPEC §5.2 明确要求「启动期暴露」。
func (r *Registry) MustRegister(c Command) {
	if err := r.Register(c); err != nil {
		panic(fmt.Sprintf("cmd: 命令注册失败: %v", err))
	}
}

// Lookup 按名字查找命令。
func (r *Registry) Lookup(name string) (Command, bool) {
	c, ok := r.byName[name]
	return c, ok
}

// Names 返回全部命令名（按注册顺序）。
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// Len 返回已注册命令数。
func (r *Registry) Len() int {
	return len(r.order)
}

// HelpText 遍历注册表生成帮助文本（SPEC §1.3：动态生成，防漂移）。
//
// 输出格式（飞书纯文本，不依赖 Markdown）：
//
//	可用命令：
//	  /help      显示本帮助
//	  /status    查看会话状态
//
// 对齐规则：命令名左对齐到最长者 + 2 空格，便于阅读。
//
// **为什么不硬编码**：硬编码的帮助文本在新增命令时必然忘记更新——
// SPEC AC-C6 用反证测试锁定这一点（改成硬编码 → 测试应变红）。
func (r *Registry) HelpText() string {
	if len(r.order) == 0 {
		return "当前没有可用命令。"
	}

	// 计算对齐宽度（按 Usage 而非 Name——Usage 含 "/" 前缀）。
	width := 0
	for _, name := range r.order {
		if u := r.byName[name].Usage; len(u) > width {
			width = len(u)
		}
	}

	var sb strings.Builder
	sb.WriteString("可用命令：\n")
	for _, name := range r.order {
		c := r.byName[name]
		usage := c.Usage
		if usage == "" {
			usage = "/" + name
		}
		desc := c.Desc
		if c.OwnerOnly {
			desc += "（仅 owner）"
		}
		fmt.Fprintf(&sb, "  %-*s  %s\n", width, usage, desc)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// SortedNames 返回排序后的命令名（供测试与日志用）。
func (r *Registry) SortedNames() []string {
	out := r.Names()
	sort.Strings(out)
	return out
}

// validateName 校验命令名格式：^[a-z_]{1,32}$。
//
// 只允许小写字母与下划线——命令名是用户可见的稳定标识，
// 限制字符集可避免大小写歧义与特殊字符注入。
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: 不能为空", ErrInvalidName)
	}
	if len(name) > maxCommandNameLen {
		return fmt.Errorf("%w: 长度 %d 超过上限 %d",
			ErrInvalidName, len(name), maxCommandNameLen)
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		isLower := ch >= 'a' && ch <= 'z'
		if !isLower && ch != '_' {
			return fmt.Errorf("%w: %q 含非法字符 %q（只允许小写字母与下划线）",
				ErrInvalidName, name, string(ch))
		}
	}
	return nil
}
