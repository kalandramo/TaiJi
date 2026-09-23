package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Load 从受信启动环境构建配置基线，再合并（可选）工作区环境文件。
//
// 顺序即安全语义：基线先建立，工作区只能补充非保留键。
// 保留键永远以 env 为准（见 MergeWorkspaceEnv）。
//
// env 为启动环境快照；workspaceFile 为空表示无工作区覆盖。
// warn 接收跳过告警，可为 nil。
func Load(env map[string]string, workspaceFile string, warn func(string)) (map[string]string, error) {
	if env == nil {
		return nil, errors.New("config: env must not be nil")
	}

	workspace, err := readWorkspaceEnv(workspaceFile)
	if err != nil {
		return nil, err
	}

	return MergeWorkspaceEnv(env, workspace, warn), nil
}

// readWorkspaceEnv 读取工作区环境文件（每行 KEY=VALUE，# 开头为注释）。
// 空路径返回空 map（无覆盖）。
//
// 注意：本函数不做任何保留键过滤——过滤是 MergeWorkspaceEnv 的职责，
// 单一职责使过滤点唯一，便于审计。
func readWorkspaceEnv(path string) (map[string]string, error) {
	if strings.TrimSpace(path) == "" {
		return map[string]string{}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read workspace env %q: %w", path, err)
	}

	out := make(map[string]string)
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("config: %s:%d: line is not KEY=VALUE: %q", path, i+1, line)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// SnapshotEnv 从进程环境构建快照 map。
func SnapshotEnv() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// FormatSorted 以稳定顺序渲染配置，用于演示与日志。
func FormatSorted(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, m[k])
	}
	return b.String()
}
