package main

import (
	"testing"
)

// 工具调用轮次上限的环境变量解析（缺口 4）。
//
// 不变量：**上限默认生效**（未设/非法值时回退默认），显式 0 才关闭。
// 这是缺口真正修好的前提——默认不生效等于缺口在新部署上依然敞开。

func TestEnvMaxToolIterations_DefaultWhenUnset(t *testing.T) {
	t.Setenv("TAIJI_MAX_TOOL_ITERATIONS", "")
	if got := envMaxToolIterations(); got != defaultMaxToolIterations {
		t.Errorf("未设时应返回默认 %d，got %d", defaultMaxToolIterations, got)
	}
}

func TestEnvMaxToolIterations_ExplicitValue(t *testing.T) {
	t.Setenv("TAIJI_MAX_TOOL_ITERATIONS", "20")
	if got := envMaxToolIterations(); got != 20 {
		t.Errorf("显式值 20 应生效，got %d", got)
	}
}

func TestEnvMaxToolIterations_ZeroDisables(t *testing.T) {
	t.Setenv("TAIJI_MAX_TOOL_ITERATIONS", "0")
	if got := envMaxToolIterations(); got != 0 {
		t.Errorf("显式 0 应关闭上限，got %d", got)
	}
}

func TestEnvMaxToolIterations_InvalidFallsBackToDefault(t *testing.T) {
	// 非法值不该让上限消失——宁可保守（fail-closed 取向）。
	t.Setenv("TAIJI_MAX_TOOL_ITERATIONS", "abc")
	if got := envMaxToolIterations(); got != defaultMaxToolIterations {
		t.Errorf("非法值应回退默认 %d，got %d", defaultMaxToolIterations, got)
	}
}

// 接线断言：CLI 与 serve 两条路径都必须传入上限（缺口 4 的接线）。
//
// 剥离注释后断言——防「注释里的字符串造成假绿」（缺口 3 的实测教训）。
func TestBothPaths_WireMaxToolIterations(t *testing.T) {
	for _, fn := range []string{"runChat", "buildPipeline"} {
		body := stripLineComments(functionBody(t, fn))
		if !contains(body, "MaxToolIterations:") {
			t.Errorf("%s 未传 MaxToolIterations——缺口 4 未接线（%s 路径无上限）", fn, fn)
		}
		if !contains(body, "envMaxToolIterations()") {
			t.Errorf("%s 未读 TAIJI_MAX_TOOL_ITERATIONS——上限无来源", fn)
		}
	}
}
