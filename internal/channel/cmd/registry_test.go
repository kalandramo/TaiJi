package cmd

import (
	"strings"
	"testing"
)

// ── Registry 测试 ──

func TestRegistry_RegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	err := r.Register(Command{
		Name: "help", Usage: "/help", Desc: "显示帮助",
		Handler: func(string) (string, error) { return "ok", nil },
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d, want 1", r.Len())
	}
	c, ok := r.Lookup("help")
	if !ok {
		t.Fatal("Lookup(help) 应命中")
	}
	if c.Desc != "显示帮助" {
		t.Errorf("Desc = %q", c.Desc)
	}
}

func TestRegistry_RejectsInvalidNames(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"", "空名"},
		{"Help", "大写"},
		{"he-lp", "连字符"},
		{"he lp", "空格"},
		{"he/lp", "斜杠"},
		{"help!", "特殊字符"},
		{strings.Repeat("a", 33), "超长（>32）"},
		{"中文", "非 ASCII"},
	}
	for _, c := range cases {
		r := NewRegistry()
		err := r.Register(Command{
			Name: c.name, Handler: func(string) (string, error) { return "", nil },
		})
		if err == nil {
			t.Errorf("Register(%q) 应失败（%s）", c.name, c.why)
		} else {
			t.Logf("✓ 拒绝 %q（%s）: %v", c.name, c.why, err)
		}
	}
}

func TestRegistry_RejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	h := func(string) (string, error) { return "", nil }
	if err := r.Register(Command{Name: "help", Handler: h}); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}
	err := r.Register(Command{Name: "help", Handler: h})
	if err == nil {
		t.Fatal("重复注册应失败")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("错误应说明重复，实际: %v", err)
	}
	t.Logf("✓ 重复注册被拒: %v", err)
}

func TestRegistry_RejectsNilHandler(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Command{Name: "help"}); err == nil {
		t.Error("nil Handler 应被拒")
	}
}

func TestRegistry_MustRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustRegister 对非法输入应 panic")
		}
	}()
	NewRegistry().MustRegister(Command{Name: "BAD"})
}

// ── HelpText 测试（AC-C1 / AC-C6）──

func TestHelpText_ListsAllRegistered(t *testing.T) {
	r := NewRegistry()
	h := func(string) (string, error) { return "", nil }
	for _, n := range []string{"help", "status", "clear", "stop"} {
		r.MustRegister(Command{Name: n, Usage: "/" + n, Desc: n + " 说明", Handler: h})
	}

	help := r.HelpText()
	for _, n := range []string{"help", "status", "clear", "stop"} {
		if !strings.Contains(help, "/"+n) {
			t.Errorf("/help 输出应含 /%s，实际:\n%s", n, help)
		}
	}
	t.Logf("✓ /help 列出全部 4 条命令:\n%s", help)
}

// AC-C6 核心：/help 必须**动态**从注册表生成。
//
// 这条测试的**反证**方式：把 HelpText 改成硬编码字符串后应 FAIL。
// 见 SPEC §9.4 的反证设计。
func TestHelpText_DynamicFromRegistry(t *testing.T) {
	r := NewRegistry()
	h := func(string) (string, error) { return "", nil }

	r.MustRegister(Command{Name: "help", Usage: "/help", Desc: "帮助", Handler: h})
	before := r.HelpText()

	// 新增一条命令——帮助文本**必须**随之变化。
	r.MustRegister(Command{Name: "brand_new", Usage: "/brand_new", Desc: "新命令", Handler: h})
	after := r.HelpText()

	if before == after {
		t.Fatal("新增命令后 /help 输出未变化——说明它是硬编码的，不是动态生成")
	}
	if !strings.Contains(after, "/brand_new") {
		t.Errorf("新增命令应出现在 /help 中，实际:\n%s", after)
	}
	t.Logf("✓ 新增命令后 /help 自动包含它（动态生成）")
}

func TestHelpText_MarksOwnerOnly(t *testing.T) {
	r := NewRegistry()
	h := func(string) (string, error) { return "", nil }
	r.MustRegister(Command{Name: "clear", Usage: "/clear", Desc: "清空", OwnerOnly: true, Handler: h})

	help := r.HelpText()
	if !strings.Contains(help, "owner") {
		t.Errorf("OwnerOnly 命令应在 /help 中标注，实际:\n%s", help)
	}
	t.Logf("✓ OwnerOnly 标注生效: %s", strings.TrimSpace(help))
}

func TestHelpText_EmptyRegistry(t *testing.T) {
	if got := NewRegistry().HelpText(); !strings.Contains(got, "没有") {
		t.Errorf("空注册表的帮助应说明无命令，实际: %q", got)
	}
}

// ── Parse 测试（AC-C4 核心）──

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	h := func(string) (string, error) { return "", nil }
	r.MustRegister(Command{Name: "help", Usage: "/help", Handler: h})
	r.MustRegister(Command{Name: "status", Usage: "/status", Handler: h})
	return r
}

func TestParse_HappyPath(t *testing.T) {
	r := newTestRegistry(t)

	cases := []struct {
		input    string
		wantName string
		wantArgs string
	}{
		{"/help", "help", ""},
		{"/help ", "help", ""},
		{"  /help  ", "help", ""},
		{"/status now", "status", "now"},
		{"/HELP", "help", ""}, // 大写归一化
		{"/Help extra args", "help", "extra args"},
	}
	for _, c := range cases {
		got := r.Parse(c.input)
		if !got.OK {
			t.Errorf("Parse(%q) 应命中", c.input)
			continue
		}
		if got.Name != c.wantName || got.Args != c.wantArgs {
			t.Errorf("Parse(%q) = (%q, %q), want (%q, %q)",
				c.input, got.Name, got.Args, c.wantName, c.wantArgs)
		} else {
			t.Logf("✓ Parse(%q) → name=%q args=%q", c.input, got.Name, got.Args)
		}
	}
}

// AC-C4 核心：未注册的 /xxx 必须走正常消息路径（不报错、不误判）。
func TestParse_UnregisteredGoesToNormalPath(t *testing.T) {
	r := newTestRegistry(t)

	cases := []struct {
		input string
		why   string
	}{
		{"/usr/local/bin 是什么", "路径被误判"},
		{"/unknown", "未注册命令"},
		{"/", "仅斜杠"},
		{"hello world", "无前缀"},
		{"", "空"},
		{"   ", "纯空白"},
		{"no /slash at start", "斜杠不在开头"},
	}
	for _, c := range cases {
		if got := r.Parse(c.input); got.OK {
			t.Errorf("Parse(%q) 不应命中（%s），实际 name=%q", c.input, c.why, got.Name)
		} else {
			t.Logf("✓ Parse(%q) → 非命令（%s）", c.input, c.why)
		}
	}
}

// 超长文本不做命令判定（SPEC §5.2）。
func TestParse_TooLongSkipsCommandDetection(t *testing.T) {
	r := newTestRegistry(t)
	long := "/help " + strings.Repeat("x", maxContentLen)
	if got := r.Parse(long); got.OK {
		t.Error("超长文本不应被判定为命令")
	}
	t.Logf("✓ 超长文本（%d 字符）跳过命令判定", len(long))
}

func TestParse_NamesIsStable(t *testing.T) {
	r := NewRegistry()
	h := func(string) (string, error) { return "", nil }
	for _, n := range []string{"zebra", "alpha", "mid"} {
		r.MustRegister(Command{Name: n, Handler: h})
	}
	// Names 保持注册顺序（不是字典序）——/help 输出稳定
	got := r.Names()
	want := []string{"zebra", "alpha", "mid"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
