package authz

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// 端到端验证：走真实的插件拒绝/放行路径，检查日志里的主体标识。
//
// 这是对"脱敏是否真的生效"的运行时验证——不是源码扫描。
func TestRedactEndToEnd_LogsAreSanitized(t *testing.T) {
	const openID = "ou_44dc01ee5577daa5ded4d42c4910f618"

	for _, tc := range []struct {
		name  string
		allow bool
	}{
		{"拒绝路径", false},
		{"放行路径", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			src := &probePermSource{allow: tc.allow}
			plugin := NewPrincipalPolicyPlugin(src, func(f string, a ...any) {
				// 记录**格式化后**的输出——只记格式串看不到实参，
				// 那样的验证是空转的（捕获不到泄漏）。
				logs = append(logs, fmt.Sprintf(f, a...))
			})

			ctx := WithPrincipal(context.Background(), Principal{
				Type: "im_user",
				ID:   "default:feishu:" + openID,
			})
			cb := plugin.beforeTool()
			if _, err := cb(ctx, &tool.BeforeToolArgs{ToolName: "mockmcp_echo"}); err != nil {
				t.Fatalf("beforeTool: %v", err)
			}

			joined := strings.Join(logs, "\n")
			if joined == "" {
				t.Fatal("未产生日志")
			}
			if strings.Contains(joined, openID) {
				t.Errorf("日志含完整 open_id:\n%s", joined)
			}
			if !strings.Contains(joined, "default:feishu:") {
				t.Errorf("日志应保留渠道段:\n%s", joined)
			}
			t.Logf("✓ %s 日志已脱敏: %s", tc.name, joined)
		})
	}
}

type probePermSource struct{ allow bool }

func (s *probePermSource) Allowed(context.Context, Principal, string) (bool, error) {
	return s.allow, nil
}
