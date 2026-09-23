package feishu

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// VerifyConfig 是验签所需凭据。
//
// 凭据来自受信启动环境，不落配置文件（NFR-9.1）。
// 依据：docs/03-原型设计文档.md:502（§4.4.2）+ FR-10.1。
type VerifyConfig struct {
	// VerificationToken 是飞书事件体的 token 比对值（webhook 模式必需）。
	VerificationToken string
	// EncryptKey 是 AES-256-CBC 的解密口令。为空时拒绝加密事件（不放行）。
	EncryptKey string
}

// Source 是飞书 webhook 入站实现。
//
// 它同时实现 channel.InboundSource（验签 + 解析 + 挑战应答）。
// 长连接模式不实现该接口——其入站由 SDK 回调直出
// （docs/03-原型设计文档.md:491 的实现矩阵）。
type Source struct {
	cfg VerifyConfig
}

// 编译期断言：Source 必须满足入站层契约。
// 这条断言的价值是把「接口与实现脱节」变成编译错误，而不是运行期惊喜。
// 依据：docs/02-设计文档.md:1536（Wave 6 接线验证要求编译期断言）。
var _ channel.InboundSource = (*Source)(nil)

// NewSource 构造飞书入站实现。
func NewSource(cfg VerifyConfig) *Source {
	return &Source{cfg: cfg}
}

// VerifyCallback 执行飞书 webhook 的两层校验，成功返回解密后的原始事件体。
//
// 层次（对齐 WeKnora internal/im/feishu/adapter.go:195-235 的实测实现）：
//
//	第一层：若 body 含 encrypt 字段 → AES-256-CBC 解密
//	第二层：比对 header.token 与配置的 VerificationToken
//
// **fail-closed 是硬语义**：凭据未配置时拒绝，不是跳过校验。
// 参照实现的血泪教训——happyclaw 旧版 `botOpenId` 为空时「安全降级=默认放行」，
// 导致 require_mention 在所有群静默失效（docs/03-原型设计文档.md:576）。
// token 缺失同理：静默放行等于把渠道边界变成摆设。
//
// 关于签名（一处明示的取舍）：飞书另有 X-Lark-Signature 签名头，但设计文档
// §4.4.2 已标注「签名算法的精确拼接顺序未逐字核实」（官方文档页为 SPA 无法抓取
// 正文）。原型采用 token 比对路径规避该不确定性。**代价**：token 随事件体传输，
// 强度依赖 TLS 与 token 保密，弱于基于密钥的签名。若后续核实了签名算法，
// 应在此处补第三层校验——接口位无需变动。
func (s *Source) VerifyCallback(r *http.Request) error {
	if s.cfg.VerificationToken == "" {
		// 不把配置值写进错误——错误会进日志，日志不该有凭据。
		return errors.New("feishu: verification token not configured (fail-closed)")
	}

	body, err := readAndRestore(r)
	if err != nil {
		return fmt.Errorf("feishu: read body: %w", err)
	}

	raw, err := s.decryptIfNeeded(body)
	if err != nil {
		return err
	}

	var ev struct {
		Header *struct {
			Token string `json:"token"`
		} `json:"header"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("feishu: unmarshal event header: %w", err)
	}
	if ev.Header == nil {
		return errors.New("feishu: event has no header")
	}

	// 常量时间比对：防时序侧信道。长度不同时 ConstantTimeCompare 立即返回 0，
	// 不逐字节泄露前缀匹配长度。
	if subtle.ConstantTimeCompare([]byte(ev.Header.Token), []byte(s.cfg.VerificationToken)) != 1 {
		return errors.New("feishu: invalid verification token")
	}

	return nil
}

// decryptIfNeeded 在 body 含 encrypt 字段时解密，否则原样返回。
//
// 判定用「含 encrypt 字段」而不是「encrypt_key 已配置」：若按后者判定，
// 配了 key 的部署收到明文事件会被当成加密体去解，失败原因会误导排查方向。
func (s *Source) decryptIfNeeded(body []byte) ([]byte, error) {
	var enc struct {
		Encrypt string `json:"encrypt"`
	}
	if err := json.Unmarshal(body, &enc); err != nil || enc.Encrypt == "" {
		return body, nil // 不是加密体，走明文路径
	}

	plain, err := decrypt(s.cfg.EncryptKey, enc.Encrypt)
	if err != nil {
		return nil, fmt.Errorf("feishu: decrypt event: %w", err)
	}
	return plain, nil
}

// decrypt 解密飞书加密事件体。
//
// 算法：AES-256-CBC，密钥 = SHA-256(encryptKey)，IV 为密文首 16 字节，
// PKCS#7 填充。依据参照实现源码 WeKnora internal/im/feishu/adapter.go:1357-1399。
//
// 所有错误路径都不回显密钥或明文片段。
func decrypt(encryptKey, encrypted string) ([]byte, error) {
	if encryptKey == "" {
		return nil, errors.New("encrypt key not configured")
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	if len(ciphertext) <= aes.BlockSize {
		// 必须 <= 而不是 <：恰好一个块时去掉 IV 后明文长度为 0，
		// 后面按 PKCS#7 解读会读到越界下标。
		return nil, errors.New("ciphertext too short")
	}

	keyHash := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(keyHash[:])
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}

	iv, data := ciphertext[:aes.BlockSize], ciphertext[aes.BlockSize:]
	if len(data)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext is not block-aligned")
	}

	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, data)

	return pkcs7Unpad(plain, aes.BlockSize)
}

// pkcs7Unpad 去除并校验 PKCS#7 填充。
//
// 校验每一位而非只看末字节：填充字节全同是 PKCS#7 的定义，
// 只查末字节会让约 1/256 的错误密钥被误判为合法（这类误判在解密路径上
// 直接等价于验签绕过）。
func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("empty plaintext")
	}

	padLen := int(b[len(b)-1])
	if padLen == 0 || padLen > blockSize || padLen > len(b) {
		return nil, errors.New("invalid padding")
	}
	for i := 0; i < padLen; i++ {
		if b[len(b)-1-i] != byte(padLen) {
			return nil, errors.New("invalid padding")
		}
	}
	return b[:len(b)-padLen], nil
}

// HandleURLVerification 应答飞书首次配置的 URL 挑战请求。
//
// 返回 true 表示该请求是挑战请求且已应答，调用方不应继续解析。
//
// 关于「挑战是否该先验签」的取舍：此处**不校验 token**，与参照实现一致
// （WeKnora adapter.go:238-283 只做解密 + challenge 提取）。理由是挑战请求
// 的用途就是把 URL 交给平台确认可达，此时凭据可能尚未在配置侧生效；
// 拒绝它会让「首次配置」这一步永久失败。**代价**：该端点可被任意调用者探测
// 是否存活并拿到它回显的 challenge——不构成信息泄露（challenge 由请求方提供），
// 但暴露端点存在性。生产部署应以网关限流覆盖该路径。
func (s *Source) HandleURLVerification(w http.ResponseWriter, r *http.Request) bool {
	body, err := readAndRestore(r)
	if err != nil {
		return false
	}

	raw, err := s.decryptIfNeeded(body)
	if err != nil {
		return false // 解密失败：当作非挑战请求，交给验签路径去拒绝
	}

	var payload struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Challenge == "" {
		return false
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// 编码失败只可能是写入失败（连接已断），此时无可挽回，忽略返回值。
	_ = json.NewEncoder(w).Encode(map[string]string{"challenge": payload.Challenge})
	return true
}

// readAndRestore 读取 body 并立即还原，使后续处理链可再次读取。
//
// 这是 §4.4.2 点明的两个细节之一：io.ReadAll 消费了 body，
// 后续 ParseCallback 会读到空（docs/03-原型设计文档.md:540）。
//
// 成功与失败路径都还原——验签失败时调用方可能仍需记录原始请求用于审计。
func readAndRestore(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("request body is nil")
	}

	body, err := io.ReadAll(r.Body)
	// 读完即关闭原始流：真实部署下 body 是网络连接，不关会挂住连接。
	_ = r.Body.Close()
	// 无论成败都还原为已读内容，保证调用方拿到的 body 与消费前一致。
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return body, nil
}
