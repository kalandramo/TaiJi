package feishu

import "github.com/kalandramo/TaiJi/internal/config"

// 渠道凭据的环境键名。
//
// 与 internal/config 的 CredentialKeys 是同一组键的两个使用点：
// config 侧负责「工作区不得覆盖」，本包负责「从受信配置读取」。
// 两处字面量的一致性由 TestCredentialEnvKeysMatchConfigGuard 锁住——
// 漂移会让「受保护」与「实际读取」脱节，形成静默的凭据缺口。
//
// **webhook 专属凭据已不再读取**（FEISHU_VERIFICATION_TOKEN / FEISHU_ENCRYPT_KEY）：
// 原型只用长连接，而长连接不验签（信任 SDK 与飞书的 TLS 通道，
// docs/03-原型设计文档.md:119）。这两个键在 config.CredentialKeys 中保留——
// 移除它们会削弱将来重加 webhook 的保护基线，且留着无成本。
// 对应的常量定义（EnvVerificationToken / EnvEncryptKey）随 webhook 实现一并删除。

// credentialEnvKeys 列出本包读取的全部凭据键，供一致性测试遍历。
//
// 必须覆盖**所有**本包从受信配置读取的凭据键——漏掉一个，
// EnsureCredentialKeysProtected 就不会检查它，该键被工作区覆盖时无人报警。
//
// 历史缺口（issue #9）：新增 FEISHU_APP_ID/FEISHU_APP_SECRET 时，
// config.CredentialKeys 加了保护，但这里没同步——自检形同虚设。
// TestCredentialEnvKeysCoverConfigGuard 现在锁定反向一致性，防复发。
var credentialEnvKeys = []string{
	EnvAppID,     // 出站与长连接（issue #9）
	EnvAppSecret, // 出站与长连接（issue #9）
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
