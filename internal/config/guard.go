// Package config 负责配置加载，并守护一条安全不变量：
// 工作区可控的配置文件不能覆盖运行时控制值。
//
// 依据设计文档 §4.6（NFR-9）：工作区是 agent 可写区域。若控制值可被工作区
// 覆盖，agent 就能通过写文件提权——改沙箱策略、模型路由或凭据引用。
// MergeWorkspaceEnv 是切断该路径的关卡。
package config

import "strings"

// ReservedKeys 是运行时控制值的保留键集合。
// 这些键只能来自受信启动环境，工作区提供的同名键一律跳过（不是覆盖）。
//
// 依据设计文档 §4.6 的六个键。原型只做一处检查（合并点），
// v1.4 NFR-9.2 要求的三处纵深防御是有意降级（见设计文档 §4.6 末尾）。
var ReservedKeys = map[string]struct{}{
	"MODEL_ROUTE":    {},
	"EXECUTION_MODE": {},
	"SANDBOX_POLICY": {},
	"CREDENTIAL_REF": {},
	"MCP_SERVERS":    {},
	"TOOL_POLICY":    {},
}

// CredentialKeys 是渠道凭据的保留键集合。
//
// 与 ReservedKeys 分列而非合并，是因为威胁模型不同：
//   - 控制值被覆盖 → 提权（改沙箱策略、模型路由）
//   - 凭据被覆盖 → 凭据劫持（把飞书回调的验签口令换成攻击者已知的值，
//     于是伪造事件能通过验签——渠道边界直接失效）
//
// 但两者的防护动作相同（工作区提供的同名键一律跳过），故在检查点合并。
//
// 依据：设计文档 §4.4.2 要求凭据来自受信启动环境（NFR-9.1）；
// 03-原型设计文档.md:917 的配置约定「所有凭据走环境变量，配置文件不落密钥」。
var CredentialKeys = map[string]struct{}{
	// 飞书 webhook 验签口令（#5）
	"FEISHU_VERIFICATION_TOKEN": {},
	// 飞书事件解密口令（#5）
	"FEISHU_ENCRYPT_KEY": {},
	// 飞书应用凭据（#9 出站与长连接使用）
	"FEISHU_APP_ID":     {},
	"FEISHU_APP_SECRET": {},
}

// ReservedPrefixes 是按键**前缀**保护的集合。
//
// 与 ReservedKeys/CredentialKeys 的精确匹配不同，这里保护的是一族键——
// 键名中含变量部分（如 server 名），无法静态枚举。
//
// TAIJI_MCP_HEADERS_<SERVER>：MCP 远程 server 的静态认证头（token/API key）。
// 属凭据，与 CredentialKeys 同一威胁模型（被工作区覆盖 → 凭据劫持，
// 攻击者可把 MCP server 的 token 换成自己已知的值）。
var ReservedPrefixes = []string{
	"TAIJI_MCP_HEADERS_",
}

// isReserved 判断键是否受保护（工作区不可提供）。
func isReserved(key string) bool {
	if _, ok := ReservedKeys[key]; ok {
		return true
	}
	if _, ok := CredentialKeys[key]; ok {
		return true
	}
	for _, p := range ReservedPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// MergeWorkspaceEnv 把工作区环境合并到受信基线上，返回新 map（不修改入参）。
//
// 规则：
//   - 保留键：只认 base 的值。工作区提供的同名键被跳过并告警。
//   - 普通键：工作区值经 sanitizeValue 清洗后进入结果。
//
// warn 可为 nil（调用方不需要告警时）。
func MergeWorkspaceEnv(base, workspace map[string]string, warn func(string)) map[string]string {
	out := make(map[string]string, len(base)+len(workspace))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range workspace {
		if isReserved(k) {
			if warn != nil {
				warn("skipping managed env variable in workspace override: " + k)
			}
			continue // 跳过，不是覆盖
		}
		out[k] = sanitizeValue(v)
	}
	return out
}

// sanitizeValue 剥离控制字符，防止环境文件结构注入。
// tab 保留（合法空白）；NUL/ESC/CR/LF 等一律剥离。
func sanitizeValue(v string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return -1
		}
		return r
	}, v)
}
