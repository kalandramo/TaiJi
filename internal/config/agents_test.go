package config

import "testing"

// TAIJI_AGENTS 的保留键保护（2026-09-30）。
//
// 理由：该变量内含每个 agent 的飞书 ak/sk，与 FEISHU_APP_ID 同一威胁模型
// ——工作区可写 ⇒ 凭据劫持（把某 agent 的 app_secret 换成攻击者已知的值，
// 该 agent 的出站调用即被劫持）。
func TestReservedKeys_TAIJI_AGENTS(t *testing.T) {
	if _, ok := ReservedKeys["TAIJI_AGENTS"]; !ok {
		t.Fatal("TAIJI_AGENTS 必须加入 ReservedKeys——它内含 ak/sk，" +
			"工作区可覆盖即等于凭据劫持")
	}
}

// 工作区提供 TAIJI_AGENTS 时必须被跳过（不是覆盖），且告警。
func TestMergeWorkspaceEnv_SkipsTAIJI_AGENTS(t *testing.T) {
	base := map[string]string{"TAIJI_AGENTS": "name=a,app_id=cli_real,app_secret=real"}
	ws := map[string]string{"TAIJI_AGENTS": "name=a,app_id=cli_evil,app_secret=evil"}

	var warned []string
	out := MergeWorkspaceEnv(base, ws, func(m string) { warned = append(warned, m) })

	if out["TAIJI_AGENTS"] != base["TAIJI_AGENTS"] {
		t.Errorf("工作区不得覆盖 TAIJI_AGENTS，got %q", out["TAIJI_AGENTS"])
	}
	if len(warned) == 0 {
		t.Error("跳过时应告警（静默跳过会让用户以为配置生效了）")
	}
}
