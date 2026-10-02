package main

import (
	"net/url"
	"strings"

	"github.com/kalandramo/TaiJi/internal/config"
)

// 控制值打印的凭据脱敏。
//
// 背景（2026-10-02 真实泄露）：printControlValues 原样打印控制值，
// 而 TAIJI_AGENTS 的值内含每个 agent 的 app_secret。
//
// 为什么修法是「脱敏」而非「不打印」：打印控制值是 #1 的 demo path 证据面
// （证明工作区文件覆盖不了保留键），且多 agent 配置的 app_id/name 部分
// 正是排障最需要的（看清配了哪几个 agent）。整条不打印会牺牲这个价值。
//
// 两层防护（缺一不可）：
//
//  1. 键名级：键名含敏感词（secret/key/token/password）→ 整值脱敏。
//     覆盖 TAIJI_MODEL_API_KEY、FEISHU_APP_SECRET、TAIJI_MCP_HEADERS_* 等。
//
//  2. 字段级：**值内部**含 key=value 形态的敏感字段 → 只脱敏该字段的值。
//     覆盖 TAIJI_AGENTS——它的键名不含敏感词，但值里嵌着 app_secret。
//     只做第 1 层会漏掉这条（这正是本次泄露的实际形态）。

// sensitiveKeyWords 是「键名即敏感」的判定词。
//
// 按子串匹配且大小写不敏感：环境变量命名风格不统一
// （APP_SECRET / apiKey / TOKEN），枚举精确名会漏。
var sensitiveKeyWords = []string{
	"secret", "key", "token", "password", "passwd", "credential",
}

// sensitiveFieldNames 是「值内部字段名即敏感」的判定词。
// 用于 TAIJI_AGENTS 这类「值里嵌凭据」的形态。
var sensitiveFieldNames = []string{
	"app_secret", "secret", "api_key", "apikey", "token", "password",
}

// redactPlaceholder 是脱敏后的占位符。
//
// 用固定串而非部分保留（如 sk-***abc）：部分保留会泄露长度与前缀，
// 而对排障无帮助（要看的是「配没配」，不是「配的具体值」）。
const redactPlaceholder = "***"

// redactControlValue 对控制值做脱敏，供启动日志安全打印。
//
// 非敏感值原样返回——脱敏不得影响排障信息的完整性。
func redactControlValue(key, value string) string {
	if value == "" {
		return ""
	}

	// 先看 config 包的权威声明（CredentialKeys + 前缀保护的族）。
	// 这层覆盖 TAIJI_MCP_HEADERS_* 这类**键名不含敏感词但按设计就是凭据**
	// 的键——单靠词表会漏（它们由 ReservedPrefixes 声明，不是靠命名约定）。
	if config.IsCredentialKey(key) || keyIsSensitive(key) {
		return redactSensitiveValue(value)
	}

	// 键名不敏感，但值内部可能嵌凭据（TAIJI_AGENTS 的 app_secret、
	// 带 userinfo 的 URL）——都要处理，不因键名不敏感而跳过。
	return redactFieldsInValue(value)
}

// keyIsSensitive 判断键名是否含敏感词（大小写不敏感）。
func keyIsSensitive(key string) bool {
	lower := strings.ToLower(key)
	for _, w := range sensitiveKeyWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// redactSensitiveValue 脱敏一个整值。
//
// URL 特殊处理：保留 scheme 与 host（排障要看连的是哪台机器），
// 只脱敏 userinfo——否则整条 URL 变成 "***"，
// 用户无法确认 base URL 配对了没有。
func redactSensitiveValue(value string) string {
	if out, changed := redactURLUserinfo(value); changed {
		return out
	}
	return redactPlaceholder
}

// redactFieldsInValue 对「含 key=value 段」或「含 URL userinfo」的值脱敏。
//
// 适用两种形态：
//
//  1. TAIJI_AGENTS 这类内嵌字段：
//     name=root,app_id=cli_aaa,app_secret=XXX;name=bill,...,app_secret=YYY
//
//  2. URL 内嵌凭据：https://user:password@host/path
//
// 分隔符 `,` 与 `;` 都处理——前者是字段分隔，后者是条目分隔。
// 非 `k=v` 形态的片段原样保留（不猜结构）。
func redactFieldsInValue(value string) string {
	// URL 先处理：它整体可能不含 `,`/`;`，若不先处理会走到下面的
	// 分段逻辑，而分段后仍带 userinfo（没被 `k=v` 匹配到）。
	if out, changed := redactURLUserinfo(value); changed {
		return out
	}

	// 切分但保留分隔符，以便原样重组（不能丢分隔符——那会改变可读性，
	// 让人误以为配置格式变了）。
	var b strings.Builder
	start := 0
	for i := 0; i <= len(value); i++ {
		if i < len(value) && value[i] != ',' && value[i] != ';' {
			continue
		}
		seg := value[start:i]
		b.WriteString(redactSegment(seg))
		if i < len(value) {
			b.WriteByte(value[i])
		}
		start = i + 1
	}
	return b.String()
}

// redactURLUserinfo 若值是一个含 userinfo 的 URL，脱敏其凭据段。
//
// 返回 changed=false 表示「不是这种形态」，调用方继续走其他处理。
// 保留 host——排障要看连的是哪台机器，整条抹掉会让 base URL
// 配错时无法从日志发现。
func redactURLUserinfo(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if !strings.Contains(trimmed, "://") {
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.User == nil {
		// 无凭据段：不是我们要处理的形态（值本身无秘密）。
		return "", false
	}
	u.User = url.User(redactPlaceholder)
	return u.String(), true
}

// redactSegment 脱敏单个 `k=v` 片段；非该形态则原样返回。
func redactSegment(seg string) string {
	k, v, ok := strings.Cut(seg, "=")
	if !ok {
		return seg
	}
	if !fieldIsSensitive(strings.TrimSpace(k)) {
		return seg
	}
	if v == "" {
		return seg // 空值无秘密，保留 `app_secret=` 让人看出「配了个空的」
	}
	return k + "=" + redactPlaceholder
}

// fieldIsSensitive 判断字段名是否敏感。
//
// 用**精确匹配**而非子串：字段名空间小且我们完全掌控，
// 子串匹配会误伤（如 app_id 不含敏感词，但 "id" 若进列表就会误判）。
func fieldIsSensitive(field string) bool {
	lower := strings.ToLower(field)
	for _, n := range sensitiveFieldNames {
		if lower == n {
			return true
		}
	}
	return false
}
