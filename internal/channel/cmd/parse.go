package cmd

import "strings"

// 命令解析（SPEC §4.2）。
//
// 解析规则：
//   - TrimSpace 后必须以 "/" 开头
//   - 首个空白分隔的 token 去掉 "/" 后转小写，作为命令名
//   - 剩余部分 TrimSpace 作为 args
//
// **关键语义**：未注册的命令名**不是错误**——它走正常消息路径
// （SPEC FR-C1：「非命令的 / 开头文本不得误伤」）。
// 用户发 "/usr/local/bin 是什么" 时，命令名 "usr/local/bin" 未注册，
// 应被当作普通消息送模型，而不是报错。

// ParseResult 是一次解析的结果。
type ParseResult struct {
	// Name 是命令名（小写，不含 "/"）。仅在 OK 为 true 时有意义。
	Name string
	// Args 是命令名之后的剩余文本（已 TrimSpace）。
	Args string
	// OK 表示这是一条**已注册**的命令。
	//
	// false 有多个原因（无 "/" 前缀、命令名非法、未注册），
	// 但对调用方是同一个语义：**走正常消息路径**。
	OK bool
}

// Parse 从消息正文解析命令（SPEC §4.2）。
//
// 返回 OK=false 时调用方应把消息当普通文本处理——**不是错误**。
func (r *Registry) Parse(content string) ParseResult {
	text := strings.TrimSpace(content)

	// 长度上限：超长文本不做命令判定（SPEC §5.2）。
	// 理由：`/` 开头的超长文本更可能是粘贴的路径或代码。
	if len(text) > maxContentLen {
		return ParseResult{}
	}

	if !strings.HasPrefix(text, "/") {
		return ParseResult{}
	}

	// 去掉前导 "/" 后按首个空白切分。
	body := text[1:]
	name, args, _ := strings.Cut(body, " ")

	// 兼容其他空白字符（tab 等）：Cut 只处理空格，
	// 故再用 Fields 做一次规范化。
	if fields := strings.Fields(body); len(fields) > 0 {
		name = fields[0]
		// args 是首个 token 之后的原文（保留内部空白）。
		if idx := strings.Index(body, name); idx >= 0 {
			rest := body[idx+len(name):]
			args = strings.TrimSpace(rest)
		}
	}

	// 命令名规范化：转小写（用户可能发 "/HELP"）。
	name = strings.ToLower(strings.TrimSpace(name))

	// 仅斜杠（"/"）→ 命令名为空 → 非命令。
	if name == "" {
		return ParseResult{}
	}

	// 未注册 → 非命令（走正常消息路径，不报错）。
	if _, ok := r.byName[name]; !ok {
		return ParseResult{}
	}

	return ParseResult{Name: name, Args: args, OK: true}
}
