package main

import (
	"os"
	"testing"
)

// CLI 路径不挂用户级权限插件（issue #6 缺口 3 的镜像修复）的回归护栏。
//
// 背景（实测确认的静默失效）：CLI 不注入 Principal，若把
// Permissions: <权限源> 传给 chat.Run，用户一配 TAIJI_RBAC，
// PrincipalPolicyPlugin 就会对所有工具调用返回「无法确认你的身份」——
// CLI 功能整体失效。
//
// 本测试用源码级断言锁住：runChat 内不得出现 `Permissions:` 实参。
// 与 allowtools_test.go 同风格（该文件已用此模式断言 buildPipeline）。
func TestRunChat_DoesNotPassPermissions(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go: %v", err)
	}
	text := string(data)

	start := indexOf(text, "func runChat(")
	if start < 0 {
		t.Fatal("runChat not found")
	}
	end := indexOf(text[start:], "\nfunc ")
	if end < 0 {
		end = len(text) - start
	}
	body := text[start : start+end]

	if contains(body, "Permissions:") {
		t.Error("runChat 传了 Permissions 实参——CLI 无 IM 身份，" +
			"挂用户级权限插件会让所有工具调用被拒（静默失效）。" +
			"用户级权限仅 serve 生效。")
	}
}
