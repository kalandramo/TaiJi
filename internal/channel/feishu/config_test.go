package feishu

import (
	"errors"
	"strings"
	"testing"

	"github.com/kalandramo/TaiJi/internal/config"
)

// 凭据链的一致性：本包读取的键必须与 config 层的保护集合同步。
// 漂移的后果是静默的——新增凭据键能读、但也能被工作区覆盖。

func TestCredentialEnvKeysMatchConfigGuard(t *testing.T) {
	for _, k := range credentialEnvKeys {
		if _, ok := config.CredentialKeys[k]; !ok {
			t.Errorf("credential key %q is read by feishu but not protected by config.CredentialKeys", k)
		}
	}
}

func TestCredentialEnvKeysCoverConfigGuard(t *testing.T) {
	// **反向**一致性：config 层保护的每个 FEISHU_* 键，本包都必须登记。
	//
	// 为什么需要反向断言：只验「本包读的 ⊆ config 保护的」时，
	// **漏登记是静默的**——新增凭据键只加 config 侧保护、忘了加进
	// credentialEnvKeys，正方向断言照样通过，而 EnsureCredentialKeysProtected
	// 不会检查该键。这正是 issue #9 的实际缺口：
	// FEISHU_APP_ID/FEISHU_APP_SECRET 加了 config 保护但漏了登记，
	// 自检形同虚设，且没有任何测试报警。
	//
	// 加上反方向后，两侧任一漂移都会失败。
	registered := make(map[string]struct{}, len(credentialEnvKeys))
	for _, k := range credentialEnvKeys {
		registered[k] = struct{}{}
	}

	for k := range config.CredentialKeys {
		// 只看本渠道命名空间的键：config 层可能为将来的渠道留了键
		// （如其它平台的凭据），那些不该由 feishu 包登记。
		if !strings.HasPrefix(k, "FEISHU_") {
			continue
		}
		if _, ok := registered[k]; !ok {
			t.Errorf("config.CredentialKeys protects %q but feishu's credentialEnvKeys "+
				"does not list it — EnsureCredentialKeysProtected would not check it", k)
		}
	}
}

func TestEnsureCredentialKeysProtected_Passes(t *testing.T) {
	if err := EnsureCredentialKeysProtected(); err != nil {
		t.Errorf("EnsureCredentialKeysProtected = %v, want nil", err)
	}
}

func TestVerifyConfigFromEnv(t *testing.T) {
	got := VerifyConfigFromEnv(map[string]string{
		EnvVerificationToken: "tok",
		EnvEncryptKey:        "key",
	})
	if got.VerificationToken != "tok" {
		t.Errorf("VerificationToken = %q, want tok", got.VerificationToken)
	}
	if got.EncryptKey != "key" {
		t.Errorf("EncryptKey = %q, want key", got.EncryptKey)
	}
}

func TestVerifyConfigFromEnv_MissingKeysAreEmpty(t *testing.T) {
	got := VerifyConfigFromEnv(map[string]string{"OTHER": "x"})
	if got.VerificationToken != "" || got.EncryptKey != "" {
		t.Errorf("got %+v, want empty credentials", got)
	}
	// 空凭据的语义由 VerifyCallback 定义：拒绝一切（fail-closed）。
	s := NewSource(got)
	if err := s.VerifyCallback(postRequest(`{}`)); err == nil {
		t.Fatal("empty credentials must not verify anything")
	}
}

func TestUnprotectedCredentialError_NamesTheKey(t *testing.T) {
	err := &UnprotectedCredentialError{Key: "SOME_KEY"}
	if err.Error() == "" {
		t.Fatal("error message is empty")
	}
	if !errors.Is(err, err) { // 保持 error 接口的可用性检查
		t.Fatal("UnprotectedCredentialError does not satisfy error")
	}
}
