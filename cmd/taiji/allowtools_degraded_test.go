package main

import (
	"reflect"
	"testing"
)

// 降级 server 的白名单剔除（真实故障第二段）。
//
// 背景：MCP server 连接失败被跳过后，它的工具不再注册。白名单里若仍有
// `{server}_tool` 条目，下游 validateAllowList 会判「未注册」并 fail-fast，
// 于是「降级」在第二道门被推翻，服务照样起不来。
//
// 判据是**前缀归属**而非「名字不存在」——这个区别是本测试的核心：
// 前者是服务的问题（该降级），后者可能是笔误（该 fail-fast）。

func TestDropAllowToolsOfDegradedServers(t *testing.T) {
	cases := []struct {
		name     string
		allow    []string
		degraded []string
		wantKept []string
		wantDrop []string
	}{
		{
			name:     "剔除降级 server 的条目（真实场景）",
			allow:    []string{"Infraverse_infraverse_dce_ip"},
			degraded: []string{"Infraverse"},
			wantKept: []string{},
			wantDrop: []string{"Infraverse_infraverse_dce_ip"},
		},
		{
			name:     "拼错的名字保留——仍走 fail-fast",
			allow:    []string{"Infraverse_typo_tool", "mockmcp_echo"},
			degraded: []string{"Infraverse"},
			wantKept: []string{"mockmcp_echo"},
			wantDrop: []string{"Infraverse_typo_tool"},
		},
		{
			name:     "前缀相似但不同 server 不误伤",
			allow:    []string{"InfraverseX_tool", "Infraverse_tool"},
			degraded: []string{"Infraverse"},
			wantKept: []string{"InfraverseX_tool"},
			wantDrop: []string{"Infraverse_tool"},
		},
		{
			name:     "无降级时不改动",
			allow:    []string{"a_b"},
			degraded: nil,
			wantKept: []string{"a_b"},
		},
		{
			name:     "多 server 降级",
			allow:    []string{"A_x", "B_y", "C_z"},
			degraded: []string{"A", "B"},
			wantKept: []string{"C_z"},
			wantDrop: []string{"A_x", "B_y"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, dropped := dropAllowToolsOfDegradedServers(tc.allow, tc.degraded)
			if !reflect.DeepEqual(kept, tc.wantKept) {
				t.Errorf("kept = %v, want %v", kept, tc.wantKept)
			}
			if !reflect.DeepEqual(dropped, tc.wantDrop) {
				t.Errorf("dropped = %v, want %v", dropped, tc.wantDrop)
			}
		})
	}
}
