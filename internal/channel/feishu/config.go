package feishu

import "github.com/kalandramo/TaiJi/internal/config"

// 渠道凭据的环境键名。
//
// 与 internal/config 的 CredentialKeys 是同一组键的两个使用点：
// config 侧负责「工作区不得覆盖」，本包负责「从受信配置读取」。
// 两处字面量的一致性由 TestCredentialEnvKeysMatchConfigGuard 锁住——
// 漂移会让「受保护」与「实际读取」脱节，形成静默的凭据缺口。
const (
	// EnvVerificationToken 是 webhook 验签口令（必需）。
	EnvVerificationToken = "FEISHU_VERIFICATION_TOKEN"
	// EnvEncryptKey 是事件解密口令（可选，配了才能收加密事件）。
	EnvEncryptKey = "FEISHU_ENCRYPT_KEY"
)

// VerifyConfigFromEnv 从受信配置快照构建验签凭据。
//
// 入参应是 config.Load 的产物——即已经过保留键过滤的配置。
// 凭据不落配置文件（NFR-9.1），只从启动环境来。
func VerifyConfigFromEnv(cfg map[string]string) VerifyConfig {
	return VerifyConfig{
		VerificationToken: cfg[EnvVerificationToken],
		EncryptKey:        cfg[EnvEncryptKey],
	}
}

// credentialEnvKeys 列出本包读取的全部凭据键，供一致性测试遍历。
//
// 必须覆盖**所有**本包从受信配置读取的凭据键——漏掉一个，
// EnsureCredentialKeysProtected 就不会检查它，该键被工作区覆盖时无人报警。
//
// 历史缺口（issue #9）：新增 FEISHU_APP_ID/FEISHU_APP_SECRET 时，
// config.CredentialKeys 加了保护，但这里没同步——自检形同虚设。
// TestCredentialEnvKeysCoverConfigGuard 现在锁定反向一致性，防复发。
var credentialEnvKeys = []string{
	EnvVerificationToken, // webhook 验签
	EnvEncryptKey,        // webhook 事件解密
	EnvAppID,             // 出站与长连接（issue #9）
	EnvAppSecret,         // 出站与长连接（issue #9）
}

// EnsureCredentialKeysProtected 断言本包读取的凭据键都受 config 层保护。
//
// 这是一条**启动期**校验：如果新增了凭据键却忘了加进 config.CredentialKeys，
// 该键就能被工作区文件覆盖——凭据劫持的缺口。这里返回错误而非静默，
// 让缺口在启动时就暴露，而不是等攻击者发现。
func EnsureCredentialKeysProtected() error {
	for _, k := range credentialEnvKeys {
		if _, ok := config.CredentialKeys[k]; !ok {
			return &UnprotectedCredentialError{Key: k}
		}
	}
	return nil
}

// UnprotectedCredentialError 表示某个凭据键未被 config 层保护。
type UnprotectedCredentialError struct{ Key string }

func (e *UnprotectedCredentialError) Error() string {
	return "feishu: credential key " + e.Key + " is not protected by config.CredentialKeys (workspace could override it)"
}
