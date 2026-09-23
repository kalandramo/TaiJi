package feishu

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 安全不变量：渠道层是唯一信任边界，验签必须 fail-closed。
// 依据：docs/03-原型设计文档.md:502（§4.4.2）+ 需求文档 FR-10.1 / AC-11。
//
// RED 目标：这些断言在 verify.go 存在之前必然失败。

const (
	testToken = "test-verification-token"
	testKey   = "test-encrypt-key"
)

// feishuEventJSON 是飞书 2.0 事件体的最小形态（明文）。
func feishuEventJSON(token, openID, chatID, text string) string {
	return `{
	  "schema": "2.0",
	  "header": {"event_id": "ev_1", "token": "` + token + `", "event_type": "im.message.receive_v1"},
	  "event": {
	    "sender": {"sender_id": {"open_id": "` + openID + `"}},
	    "message": {"message_id": "om_1", "chat_id": "` + chatID + `", "chat_type": "p2p", "content": "{\"text\":\"` + text + `\"}"}
	  }
	}`
}

// encryptFeishuBody 按飞书算法加密：AES-256-CBC，密钥 = SHA-256(encryptKey)，
// IV 前置在密文头部，PKCS#7 填充，整体 base64。
//
// 算法依据是参照实现的源码（WeKnora internal/im/feishu/adapter.go:1357-1399），
// 不是被测代码的镜像——本函数用标准库原语独立实现加密方向。
func encryptFeishuBody(t *testing.T, encryptKey, plaintext string) string {
	t.Helper()

	keyHash := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(keyHash[:])
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}

	iv := make([]byte, aes.BlockSize)
	for i := range iv {
		iv[i] = byte(i) // 固定 IV，测试可复现
	}

	padded := pkcs7Pad([]byte(plaintext), aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)

	return base64.StdEncoding.EncodeToString(append(iv, out...))
}

func pkcs7Pad(b []byte, blockSize int) []byte {
	pad := blockSize - len(b)%blockSize
	return append(b, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func postRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/webhook/feishu", strings.NewReader(body))
}

// --- fail-closed：凭据缺失必须拒绝，不是跳过 ---

func TestVerifyCallback_MissingTokenFailsClosed(t *testing.T) {
	s := NewSource(VerifyConfig{EncryptKey: testKey})
	err := s.VerifyCallback(postRequest(feishuEventJSON(testToken, "ou_1", "", "hi")))
	if err == nil {
		// 这是 AC「未配置 verification token 时端点拒绝所有回调」的单测面。
		// 参照实现的历史教训：botOpenId 为空时「安全降级=放行」导致门禁静默失效
		// （docs/03-原型设计文档.md:576）。token 缺失同理，必须拒绝。
		t.Fatal("VerifyCallback with no configured token = nil, want error (fail-closed)")
	}
}

func TestVerifyCallback_EmptyTokenInEventFails(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	// header.token 为空串 —— 空串不等于「无校验」，必须拒绝
	err := s.VerifyCallback(postRequest(feishuEventJSON("", "ou_1", "", "hi")))
	if err == nil {
		t.Fatal("VerifyCallback with empty event token = nil, want error")
	}
}

// --- 第一层：token 比对 ---

func TestVerifyCallback_WrongTokenFails(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	err := s.VerifyCallback(postRequest(feishuEventJSON("forged-token", "ou_1", "", "hi")))
	if err == nil {
		t.Fatal("VerifyCallback with forged token = nil, want error (AC: 伪造 token → 403)")
	}
}

func TestVerifyCallback_CorrectTokenPasses(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	if err := s.VerifyCallback(postRequest(feishuEventJSON(testToken, "ou_1", "", "hi"))); err != nil {
		t.Fatalf("VerifyCallback with correct token = %v, want nil", err)
	}
}

func TestVerifyCallback_MissingHeaderFails(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	if err := s.VerifyCallback(postRequest(`{"schema":"2.0"}`)); err == nil {
		t.Fatal("VerifyCallback with no header = nil, want error")
	}
}

func TestVerifyCallback_MalformedJSONFails(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	if err := s.VerifyCallback(postRequest(`{not json`)); err == nil {
		t.Fatal("VerifyCallback with malformed JSON = nil, want error")
	}
}

func TestVerifyCallback_ErrorDoesNotLeakToken(t *testing.T) {
	// 错误消息会进日志。把期望的 token 写进日志等于把凭据写进日志。
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	err := s.VerifyCallback(postRequest(feishuEventJSON("forged", "ou_1", "", "hi")))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error %q leaks the configured verification token", err.Error())
	}
}

// --- 第二层：AES-256-CBC 解密 ---

func TestVerifyCallback_EncryptedEventPasses(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken, EncryptKey: testKey})
	plain := feishuEventJSON(testToken, "ou_1", "", "hi")
	body := `{"encrypt":"` + encryptFeishuBody(t, testKey, plain) + `"}`

	if err := s.VerifyCallback(postRequest(body)); err != nil {
		t.Fatalf("VerifyCallback with encrypted event = %v, want nil", err)
	}
}

func TestVerifyCallback_EncryptedWithWrongKeyFails(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken, EncryptKey: testKey})
	plain := feishuEventJSON(testToken, "ou_1", "", "hi")
	// 用另一把 key 加密 → 解密出的明文无意义 → token 比对必然失败
	body := `{"encrypt":"` + encryptFeishuBody(t, "another-key", plain) + `"}`

	if err := s.VerifyCallback(postRequest(body)); err == nil {
		t.Fatal("VerifyCallback with wrong encrypt key = nil, want error (AC: 解密失败 → 403)")
	}
}

func TestVerifyCallback_EncryptedButNoEncryptKeyFails(t *testing.T) {
	// 配了 token 但没配 encrypt_key，却收到加密事件 —— 不能当明文放行
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	plain := feishuEventJSON(testToken, "ou_1", "", "hi")
	body := `{"encrypt":"` + encryptFeishuBody(t, testKey, plain) + `"}`

	if err := s.VerifyCallback(postRequest(body)); err == nil {
		t.Fatal("VerifyCallback with encrypted body and no encrypt key = nil, want error")
	}
}

// --- body 还原：后续解析链必须能再读 ---

func TestVerifyCallback_RestoresBody(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	original := feishuEventJSON(testToken, "ou_1", "", "hi")
	r := postRequest(original)

	if err := s.VerifyCallback(r); err != nil {
		t.Fatalf("VerifyCallback: %v", err)
	}

	// AC: 解析后 HTTP body 可被再次读取（供后续处理链复用，不是消费后为空）
	again, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("re-read body: %v", err)
	}
	if string(again) != original {
		t.Errorf("body after verify = %q, want original %q", again, original)
	}
}

func TestVerifyCallback_RestoresBodyOnFailure(t *testing.T) {
	// 验签失败时 body 也应还原——调用方可能需要记录原始请求（审计）。
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	original := feishuEventJSON("forged", "ou_1", "", "hi")
	r := postRequest(original)

	_ = s.VerifyCallback(r)

	again, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("re-read body: %v", err)
	}
	if string(again) != original {
		t.Errorf("body after failed verify = %q, want original %q", again, original)
	}
}

// --- decrypt 单元面 ---

func TestDecrypt_RoundTrip(t *testing.T) {
	plain := `{"header":{"token":"t"}}`
	enc := encryptFeishuBody(t, testKey, plain)

	got, err := decrypt(testKey, enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(got) != plain {
		t.Errorf("decrypt = %q, want %q", got, plain)
	}
}

func TestDecrypt_EmptyKeyFails(t *testing.T) {
	if _, err := decrypt("", encryptFeishuBody(t, testKey, "x")); err == nil {
		t.Fatal("decrypt with empty key = nil, want error")
	}
}

func TestDecrypt_InvalidBase64Fails(t *testing.T) {
	if _, err := decrypt(testKey, "!!!not-base64!!!"); err == nil {
		t.Fatal("decrypt with invalid base64 = nil, want error")
	}
}

func TestDecrypt_ShortCiphertextFails(t *testing.T) {
	// 比 IV 还短 —— 不能 panic，必须报错
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := decrypt(testKey, short); err == nil {
		t.Fatal("decrypt with short ciphertext = nil, want error")
	}
}

func TestDecrypt_InvalidPaddingFails(t *testing.T) {
	// 构造合法长度但填充非法：末字节声明 0x00 填充
	keyHash := sha256.Sum256([]byte(testKey))
	block, _ := aes.NewCipher(keyHash[:])
	iv := make([]byte, aes.BlockSize)
	plain := make([]byte, aes.BlockSize) // 全 0 → padLen=0，非法
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, plain)
	enc := base64.StdEncoding.EncodeToString(append(iv, out...))

	if _, err := decrypt(testKey, enc); err == nil {
		t.Fatal("decrypt with invalid padding = nil, want error")
	}
}

// --- URL 挑战 ---

func TestHandleURLVerification_AnswersChallenge(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	body := `{"challenge":"ch_abc123","token":"` + testToken + `","type":"url_verification"}`
	r := postRequest(body)
	w := httptest.NewRecorder()

	if !s.HandleURLVerification(w, r) {
		t.Fatal("HandleURLVerification = false, want true for challenge request")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v (body=%q)", err, w.Body.String())
	}
	if got["challenge"] != "ch_abc123" {
		t.Errorf("challenge = %q, want %q", got["challenge"], "ch_abc123")
	}
}

func TestHandleURLVerification_EncryptedChallenge(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken, EncryptKey: testKey})
	plain := `{"challenge":"ch_enc","token":"` + testToken + `","type":"url_verification"}`
	r := postRequest(`{"encrypt":"` + encryptFeishuBody(t, testKey, plain) + `"}`)
	w := httptest.NewRecorder()

	if !s.HandleURLVerification(w, r) {
		t.Fatal("HandleURLVerification = false, want true for encrypted challenge")
	}
	if !strings.Contains(w.Body.String(), "ch_enc") {
		t.Errorf("body = %q, want it to contain the challenge", w.Body.String())
	}
}

func TestHandleURLVerification_NonChallengeReturnsFalse(t *testing.T) {
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	r := postRequest(feishuEventJSON(testToken, "ou_1", "", "hi"))
	w := httptest.NewRecorder()

	if s.HandleURLVerification(w, r) {
		t.Fatal("HandleURLVerification = true for a normal event, want false")
	}
}

func TestHandleURLVerification_RestoresBodyWhenNotChallenge(t *testing.T) {
	// 返回 false 后调用方要继续走验签 + 解析，body 必须完好
	s := NewSource(VerifyConfig{VerificationToken: testToken})
	original := feishuEventJSON(testToken, "ou_1", "", "hi")
	r := postRequest(original)
	w := httptest.NewRecorder()

	_ = s.HandleURLVerification(w, r)

	again, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("re-read body: %v", err)
	}
	if string(again) != original {
		t.Errorf("body = %q, want original %q", again, original)
	}
}
