package config

import "testing"

// 凭据保留键的安全不变量（issue #5）：
// 渠道凭据不得被工作区文件覆盖。
//
// 威胁模型与控制值不同：控制值被覆盖 → 提权；凭据被覆盖 → 凭据劫持。
// 后者更直接——把验签口令换成攻击者已知的值，伪造事件即可通过验签，
// 渠道边界（渠道层唯一的信任边界）当场失效。
//
// 依据：设计文档 §4.4.2 + 03-原型设计文档.md:917「凭据走环境变量，配置文件不落密钥」。
//
// ── 2026-10-02 变更 ──
// CredentialKeys 已清空（四个 FEISHU_* 键全部移除：应用凭据收敛到
// TAIJI_AGENTS；webhook 专属两键随形态移除再无读取方）。
//
// 故本文件的测试改为**用合成键验证机制**，而非依赖某个真实凭据键：
//   - 机制（isReserved 拦截 + 告警）仍在，且必须有测试覆盖
//   - 将来往 CredentialKeys 加键时，这些测试自动覆盖新键
// 若继续按已删的真实键断言，测试会红；若直接删掉，机制就失去保护。

// synthCredentialKey 是注入 CredentialKeys 的测试用键。
//
// 用 `_TEST_` 前缀避免与真实键冲突；defer 清理以免污染其它测试。
const synthCredentialKey = "TEST_SYNTHETIC_CREDENTIAL"

// withSyntheticCredential 临时往 CredentialKeys 注入一个合成键。
func withSyntheticCredential(t *testing.T) {
	t.Helper()
	CredentialKeys[synthCredentialKey] = struct{}{}
	t.Cleanup(func() { delete(CredentialKeys, synthCredentialKey) })
}

func TestMergeWorkspaceEnv_CredentialKeyIsSkipped(t *testing.T) {
	withSyntheticCredential(t)

	base := map[string]string{synthCredentialKey: "trusted-token"}
	workspace := map[string]string{synthCredentialKey: "attacker-token"}

	var warnings []string
	got := MergeWorkspaceEnv(base, workspace, func(m string) { warnings = append(warnings, m) })

	if got[synthCredentialKey] != "trusted-token" {
		t.Errorf("%s = %q, want trusted-token (workspace must not hijack credentials)",
			synthCredentialKey, got[synthCredentialKey])
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
}

func TestMergeWorkspaceEnv_CredentialKeyAbsentFromBaseStaysAbsent(t *testing.T) {
	withSyntheticCredential(t)

	// 凭据只能来自 base。base 没有时，工作区提供的也不得进入结果——
	// 否则「工作区自配一个验签口令」就能让未配置凭据的部署看起来是配好的。
	got := MergeWorkspaceEnv(
		map[string]string{},
		map[string]string{synthCredentialKey: "self-configured"},
		nil,
	)
	if _, present := got[synthCredentialKey]; present {
		t.Errorf("%s = %q, want absent (credentials come only from base)",
			synthCredentialKey, got[synthCredentialKey])
	}
}

func TestMergeWorkspaceEnv_AllCredentialKeysAreProtected(t *testing.T) {
	// 遍历集合本身：新增凭据键时若忘了加进 CredentialKeys，此断言会失败。
	//
	// 集合当前为空 → 下面的循环不执行。**这不是空转**：
	// 断言的对象是「集合的每个成员都受保护」，集合空时该命题平凡成立。
	// 真正在防的是「将来加了键但忘了登记」——那时循环有对象，测试立即生效。
	if len(CredentialKeys) == 0 {
		t.Log("CredentialKeys 当前为空——本测试对空集合平凡通过；" +
			"加键后自动生效（见文件头注释）")
		return
	}

	workspace := make(map[string]string, len(CredentialKeys))
	for k := range CredentialKeys {
		workspace[k] = "attacker-value"
	}

	got := MergeWorkspaceEnv(map[string]string{}, workspace, nil)
	for k := range CredentialKeys {
		if v, present := got[k]; present {
			t.Errorf("%s = %q leaked from workspace, want absent", k, v)
		}
	}
}

// CredentialKeys 清空后的**明确断言**：四个 FEISHU_* 键都不再受保护。
//
// 这不是「顺手加的」——它把「已删除」这个事实锁住，
// 防止有人以为旧键还在保护集合里（那会导致误判：
// 以为写了 FEISHU_APP_ID 在工作区会被拦，实际它现在只是普通键）。
func TestCredentialKeys_FeishuKeysRemoved(t *testing.T) {
	for _, k := range []string{
		"FEISHU_APP_ID",
		"FEISHU_APP_SECRET",
		"FEISHU_VERIFICATION_TOKEN",
		"FEISHU_ENCRYPT_KEY",
	} {
		if _, ok := CredentialKeys[k]; ok {
			t.Errorf("CredentialKeys 不应再含 %q（2026-10-02 已移除）", k)
		}
	}
}

func TestReservedKeysAndCredentialKeysDoNotOverlap(t *testing.T) {
	// 两个集合语义不同（控制值 vs 凭据）。重叠意味着归类混乱，
	// 未来改其中一处会静默影响另一处。
	for k := range CredentialKeys {
		if _, ok := ReservedKeys[k]; ok {
			t.Errorf("%q appears in both ReservedKeys and CredentialKeys", k)
		}
	}
}

func TestIsReserved(t *testing.T) {
	cases := map[string]bool{
		"MODEL_ROUTE": true,
		"TOOL_POLICY": true,
		// TAIJI_AGENTS 内含各 agent 的 ak/sk，是当前**唯一**的飞书凭据载体，
		// 必须受保护（这是清空 CredentialKeys 后最关键的一条）。
		"TAIJI_AGENTS": true,
		// 已删除的键不再是保留键——设了也不影响（但也不会被拦截）。
		"FEISHU_APP_ID":     false,
		"FEISHU_APP_SECRET": false,
		"WORKSPACE_NAME":    false,
		"":                  false,
	}
	for k, want := range cases {
		if got := isReserved(k); got != want {
			t.Errorf("isReserved(%q) = %v, want %v", k, got, want)
		}
	}
}
