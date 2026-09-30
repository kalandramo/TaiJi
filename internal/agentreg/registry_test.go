package agentreg

import (
	"errors"
	"testing"
)

func TestNew_EmptyReturnsNil(t *testing.T) {
	r, err := New(nil)
	if err != nil {
		t.Fatalf("空列表不应报错: %v", err)
	}
	if r != nil {
		t.Error("空列表应返回 nil（表示未启用多 agent）")
	}
	// nil 注册表上调用 Resolve 不得 panic，且返回空 agent。
	agent, err := r.Resolve("cli_anything")
	if err != nil || agent != "" {
		t.Errorf("nil 注册表应返回空 agent 且无错误，got (%q, %v)", agent, err)
	}
}

func TestResolve_MapsAppIDToAgent(t *testing.T) {
	r, err := New([]Entry{
		{AppID: "cli_aaa", Agent: "billing"},
		{AppID: "cli_bbb", Agent: "ops"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := map[string]string{"cli_aaa": "billing", "cli_bbb": "ops"}
	for appID, want := range cases {
		got, err := r.Resolve(appID)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", appID, err)
		}
		if got != want {
			t.Errorf("Resolve(%s) = %q, want %q", appID, got, want)
		}
	}
}

// 未知 AppID 必须 fail-closed（返回错误，不落到默认 agent）。
func TestResolve_UnknownAppIsError(t *testing.T) {
	r, _ := New([]Entry{{AppID: "cli_aaa", Agent: "billing"}})

	got, err := r.Resolve("cli_unknown")
	if err == nil {
		t.Fatal("未知 app_id 应返回错误（fail-closed），不能静默落到默认 agent——" +
			"否则新 bot 配错会把消息喂给错误 agent")
	}
	if !errors.Is(err, ErrUnknownApp) {
		t.Errorf("错误应可用 errors.Is 匹配 ErrUnknownApp，got %v", err)
	}
	if got != "" {
		t.Errorf("出错时不应返回 agent 名，got %q", got)
	}
}

func TestNew_DuplicateAppIDFailsFast(t *testing.T) {
	_, err := New([]Entry{
		{AppID: "cli_same", Agent: "a"},
		{AppID: "cli_same", Agent: "b"},
	})
	if err == nil {
		t.Fatal("重复 app_id 应报错（同一应用无法映射到两个 agent）")
	}
}

func TestNew_MissingFieldsFailFast(t *testing.T) {
	if _, err := New([]Entry{{AppID: "", Agent: "a"}}); err == nil {
		t.Error("空 app_id 应报错")
	}
	if _, err := New([]Entry{{AppID: "cli_x", Agent: ""}}); err == nil {
		t.Error("空 agent 名应报错")
	}
}

func TestAgents_ReturnsSortedUnique(t *testing.T) {
	r, _ := New([]Entry{
		{AppID: "cli_1", Agent: "ops"},
		{AppID: "cli_2", Agent: "billing"},
	})
	got := r.Agents()
	if len(got) != 2 || got[0] != "billing" || got[1] != "ops" {
		t.Errorf("Agents() = %v, want [billing ops]", got)
	}
	if r.Len() != 2 {
		t.Errorf("Len() = %d, want 2", r.Len())
	}
}
