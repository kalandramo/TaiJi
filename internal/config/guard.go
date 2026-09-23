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
		if _, reserved := ReservedKeys[k]; reserved {
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
