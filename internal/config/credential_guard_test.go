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

func TestMergeWorkspaceEnv_CredentialKeyIsSkipped(t *testing.T) {
	base := map[string]string{"FEISHU_VERIFICATION_TOKEN": "trusted-token"}
	workspace := map[string]string{"FEISHU_VERIFICATION_TOKEN": "attacker-token"}

	var warnings []string
	got := MergeWorkspaceEnv(base, workspace, func(m string) { warnings = append(warnings, m) })

	if got["FEISHU_VERIFICATION_TOKEN"] != "trusted-token" {
		t.Errorf("FEISHU_VERIFICATION_TOKEN = %q, want trusted-token (workspace must not hijack credentials)",
			got["FEISHU_VERIFICATION_TOKEN"])
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
}

func TestMergeWorkspaceEnv_CredentialKeyAbsentFromBaseStaysAbsent(t *testing.T) {
	// 凭据只能来自 base。base 没有时，工作区提供的也不得进入结果——
	// 否则「工作区自配一个验签口令」就能让未配置凭据的部署看起来是配好的。
	got := MergeWorkspaceEnv(
		map[string]string{},
		map[string]string{"FEISHU_VERIFICATION_TOKEN": "self-configured"},
		nil,
	)
	if _, present := got["FEISHU_VERIFICATION_TOKEN"]; present {
		t.Errorf("FEISHU_VERIFICATION_TOKEN = %q, want absent (credentials come only from base)",
			got["FEISHU_VERIFICATION_TOKEN"])
	}
}

func TestMergeWorkspaceEnv_AllCredentialKeysAreProtected(t *testing.T) {
	// 遍历集合本身：新增凭据键时若忘了加进 CredentialKeys，此断言会失败。
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

func TestCredentialKeys_ContainsFeishuWebhookCredentials(t *testing.T) {
	// issue #5 的验签与解密依赖这两个键；缺失则渠道边界无法建立。
	for _, k := range []string{"FEISHU_VERIFICATION_TOKEN", "FEISHU_ENCRYPT_KEY"} {
		if _, ok := CredentialKeys[k]; !ok {
			t.Errorf("CredentialKeys missing %q", k)
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
		"MODEL_ROUTE":               true,
		"TOOL_POLICY":               true,
		"FEISHU_VERIFICATION_TOKEN": true,
		"FEISHU_ENCRYPT_KEY":        true,
		"WORKSPACE_NAME":            false,
		"":                          false,
	}
	for k, want := range cases {
		if got := isReserved(k); got != want {
			t.Errorf("isReserved(%q) = %v, want %v", k, got, want)
		}
	}
}
