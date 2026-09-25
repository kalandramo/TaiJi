package main

import (
	"os"
	"strings"
	"testing"
)

// serve 路径的权限配置一致性校验（issue #6 缺口 3）。
//
// 不变量：**有工具可调用时，必须有明确的用户级权限决策**——
// 要么配了权限表（按用户管控），要么显式声明放开（TAIJI_ALLOW_ALL_USERS=1）。
// 二者皆无 → 拒绝启动（fail-fast），而非运行期静默放行。
//
// 为什么 fail-fast 而非运行期拒绝：这是**配置错误**，不是运行时状态。
// 启动期大声失败优于运行期静默放行——静默失效最难排查（issue #9 教训）。
//
// 为什么只在 serve 校验：CLI 无 IM 身份，用户级权限不适用（见 runChat 的注释）。

func TestValidateServePermissions_ToolsWithoutPolicyIsError(t *testing.T) {
	// 有工具 + 未配权限表 + 未显式放开 → 拒绝启动。
	err := validateServePermissions(servePermInput{
		HasTools: true, HasPermissions: false, AllowAllUsers: false,
	})
	if err == nil {
		t.Fatal("有工具但无权限决策应拒绝启动，实际放行（缺口 3）")
	}
	// 错误信息必须给出两条出路，否则用户不知如何修复。
	for _, want := range []string{"TAIJI_USER_PERMISSIONS", "TAIJI_ALLOW_ALL_USERS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应提示 %s，实际：%v", want, err)
		}
	}
}

func TestValidateServePermissions_NoToolsIsOK(t *testing.T) {
	// 无工具可调用 → 无安全边界可失，放行。
	err := validateServePermissions(servePermInput{
		HasTools: false, HasPermissions: false, AllowAllUsers: false,
	})
	if err != nil {
		t.Errorf("无工具时应放行，实际：%v", err)
	}
}

func TestValidateServePermissions_WithPolicyIsOK(t *testing.T) {
	// 配了权限表 → 放行。
	err := validateServePermissions(servePermInput{
		HasTools: true, HasPermissions: true, AllowAllUsers: false,
	})
	if err != nil {
		t.Errorf("配了权限表应放行，实际：%v", err)
	}
}

func TestValidateServePermissions_ExplicitAllowAllIsOK(t *testing.T) {
	// 显式放开 → 放行（这是有意为之的决策，不是遗漏）。
	err := validateServePermissions(servePermInput{
		HasTools: true, HasPermissions: false, AllowAllUsers: true,
	})
	if err != nil {
		t.Errorf("显式放开应放行，实际：%v", err)
	}
}

func TestValidateServePermissions_PolicyAndAllowAllBothOK(t *testing.T) {
	// 两者都设 → 权限表优先（更严），仍放行。
	err := validateServePermissions(servePermInput{
		HasTools: true, HasPermissions: true, AllowAllUsers: true,
	})
	if err != nil {
		t.Errorf("两者都设应放行（权限表优先），实际：%v", err)
	}
}

// 接线断言：buildPipeline 必须真的调用 validateServePermissions。
//
// 这是缺口 1 的教训——「有定义无调用」是最难发现的静默失效
// （RequireWritable 曾如此）。源码级断言成本低、能锁住接线。
//
// **为什么先剥离注释**（实测教训）：初版直接 contains(body, "validateServePermissions(")，
// 反证时把调用注释掉，断言仍 PASS——因为注释里也含该字符串（假绿）。
// 剥离注释后，注释掉的调用不再命中，护栏才真正有效。
func TestBuildPipeline_WiresServePermissionValidation(t *testing.T) {
	body := functionBody(t, "buildPipeline")
	code := stripLineComments(body)

	if !contains(code, "validateServePermissions(") {
		t.Error("buildPipeline 未调用 validateServePermissions——" +
			"缺口 3 的 fail-fast 未接线（有定义无调用）")
	}
	if !contains(code, "allowAllUsersFromEnv()") {
		t.Error("buildPipeline 未读取 TAIJI_ALLOW_ALL_USERS——" +
			"显式放开的通道缺失")
	}
}

// functionBody 返回 main.go 中某函数的源码段（从 func 名到下一个 func）。
func functionBody(t *testing.T, funcName string) string {
	t.Helper()
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go: %v", err)
	}
	text := string(data)
	start := indexOf(text, "func "+funcName+"(")
	if start < 0 {
		t.Fatalf("%s not found", funcName)
	}
	end := indexOf(text[start:], "\nfunc ")
	if end < 0 {
		end = len(text) - start
	}
	return text[start : start+end]
}

// stripLineComments 移除行注释（含行尾注释）。
//
// 近似实现：逐行去掉 `//` 之后的部分。对本项目的断言足够
// （不处理字符串字面量里的 `//`——这里断言的代码段无此情形）。
func stripLineComments(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if i := strings.Index(ln, "//"); i >= 0 {
			ln = ln[:i]
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
