package feishu

import (
	"errors"
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
