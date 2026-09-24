package authz

import (
	"os"
	"strings"
	"testing"
)

// 主体 ID 的日志脱敏。
//
// 动机：open_id 是飞书用户的隐私标识，不应落到日志。但日志又需要
// 能区分不同用户（审计、排障）——故保留渠道段、只对用户段做不可逆摘要。
//
// 形态：{workspace}:{platform}:{open_id}
//   →   {workspace}:{platform}:{hash8}
//
// 保留前两段：它们不含隐私，且是排障必需的上下文（哪个工作区、哪个平台）。

func TestRedacted_HidesOpenID(t *testing.T) {
	p := Principal{Type: "im_user", ID: "default:feishu:ou_44dc01ee5577daa5ded4d42c4910f618"}

	got := p.Redacted()

	if strings.Contains(got, "ou_44dc01ee5577daa5ded4d42c4910f618") {
		t.Errorf("脱敏后仍含完整 open_id: %q", got)
	}
	if strings.Contains(got, "ou_") {
		t.Errorf("脱敏后不应保留 open_id 前缀: %q", got)
	}
}

func TestRedacted_KeepsChannelContext(t *testing.T) {
	p := Principal{Type: "im_user", ID: "default:feishu:ou_44dc01ee5577daa5ded4d42c4910f618"}

	got := p.Redacted()

	// 前两段保留——排障需要知道哪个工作区、哪个平台
	if !strings.HasPrefix(got, "default:feishu:") {
		t.Errorf("应保留渠道段，got %q", got)
	}
}

// 同一主体多次调用 → 同一摘要（日志可关联同一用户的操作）。
func TestRedacted_StableAcrossCalls(t *testing.T) {
	p := Principal{Type: "im_user", ID: "default:feishu:ou_alice"}

	first := p.Redacted()
	for i := 0; i < 5; i++ {
		if got := p.Redacted(); got != first {
			t.Fatalf("第 %d 次调用结果不一致: %q vs %q", i+1, got, first)
		}
	}
}

// 不同主体 → 不同摘要（日志能区分用户）。
func TestRedacted_DistinguishesPrincipals(t *testing.T) {
	a := Principal{Type: "im_user", ID: "default:feishu:ou_alice"}
	b := Principal{Type: "im_user", ID: "default:feishu:ou_bob"}

	if a.Redacted() == b.Redacted() {
		t.Errorf("不同主体得到相同摘要: %q", a.Redacted())
	}
}

// 空主体 → 空串（不产生"匿名用户"的假标识）。
func TestRedacted_EmptyPrincipal(t *testing.T) {
	if got := (Principal{}).Redacted(); got != "" {
		t.Errorf("空主体应返回空串，got %q", got)
	}
}

// 形态异常（段数不足）→ 整串摘要，不泄漏原文。
func TestRedacted_MalformedIDStillHides(t *testing.T) {
	p := Principal{Type: "im_user", ID: "ou_bare_openid_no_colon"}

	got := p.Redacted()

	if strings.Contains(got, "ou_bare_openid_no_colon") {
		t.Errorf("形态异常时仍不得泄漏原文: %q", got)
	}
}

// 摘要是定长的（不随输入长度变化而泄漏长度信息）。
func TestRedacted_HashLengthStable(t *testing.T) {
	short := Principal{ID: "a:b:c"}.Redacted()
	long := Principal{ID: "aaaaaaaaaaaaaaaaaaaa:bbbbbbbbbbbbbbbbbbbb:cccccccccccccccccccc"}.Redacted()

	// 取出最后一段（摘要）比较长度
	sh := short[strings.LastIndex(short, ":")+1:]
	lh := long[strings.LastIndex(long, ":")+1:]
	if len(sh) != len(lh) {
		t.Errorf("摘要长度应固定: %d vs %d（%q / %q）", len(sh), len(lh), short, long)
	}
}

// 防回归：权限插件的日志**不得**直接打印 principal.ID。
//
// 为什么用源码级断言：日志内容难以从外部观测（需要构造完整的
// runner + 工具调用链才能触发）。而这里要守的是一条静态事实——
// "日志调用里没有 principal.ID"。源码扫描比运行时断言更直接可靠。
//
// 反向验证：把任一 p.log 的参数改回 principal.ID，本测试即 FAIL。
func TestPermissionPlugin_LogsNeverUseRawPrincipalID(t *testing.T) {
	src, err := os.ReadFile("permission_plugin.go")
	if err != nil {
		t.Fatalf("read permission_plugin.go: %v", err)
	}
	text := string(src)

	if strings.Contains(text, "principal.ID") {
		t.Error("permission_plugin.go 含 principal.ID —— " +
			"open_id 是隐私数据，日志必须用 principal.Redacted()")
	}
	// 至少要有脱敏调用（防止把日志整个删掉也算通过）
	if !strings.Contains(text, "principal.Redacted()") {
		t.Error("permission_plugin.go 未见 principal.Redacted() —— 日志可能被误删")
	}
}
