package feishu

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// DefaultWebhookPath 是飞书事件订阅的默认路径。
// 部署时在飞书开放平台配置事件订阅 URL 为 http://<host>:8080/webhook/feishu。
const DefaultWebhookPath = "/webhook/feishu"

// HandlerConfig 是 webhook 处理器的装配参数。
type HandlerConfig struct {
	// Verify 是验签凭据。凭据来自受信启动环境（NFR-9.1）。
	Verify VerifyConfig

	// Path 是挂载路径，空则用 DefaultWebhookPath。
	Path string

	// OnMessage 是解析出消息后的下游回调。
	//
	// 为 nil 表示「只验签不处理」——允许这种装配是刻意的：
	// 它让「端点是否 fail-closed」可以独立于下游接线被验证（见 issue #5 的
	// Demo path——只要求端点正确应答并打印日志）。
	//
	// 返回 error 会让端点回 500。下游不可用时不能让飞书认为投递成功，
	// 否则飞书不再重试，消息永久丢失。
	OnMessage func(*channel.IncomingMessage) error

	// Logf 是日志出口，nil 则用标准库 log。
	Logf func(format string, args ...any)
}

// Handler 是飞书 webhook 的 HTTP 入口。
//
// 它串起 issue #5 的端到端链路：
//
//	飞书 POST → 挑战判定 → 验签+解密 → 解析为 IncomingMessage → 下游回调
//
// 安全语义：验签失败必须 403，且不得产生任何 IncomingMessage。
type Handler struct {
	cfg       VerifyConfig
	source    *Source
	path      string
	onMessage func(*channel.IncomingMessage) error
	logf      func(format string, args ...any)
}

// NewHandler 构造 webhook 处理器。
func NewHandler(cfg HandlerConfig) *Handler {
	path := cfg.Path
	if path == "" {
		path = DefaultWebhookPath
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Handler{
		cfg:       cfg.Verify,
		source:    NewSource(cfg.Verify),
		path:      path,
		onMessage: cfg.OnMessage,
		logf:      logf,
	}
}

// Path 返回处理器挂载路径，供 mux 注册使用。
func (h *Handler) Path() string { return h.path }

// ServeHTTP 实现 http.Handler。
//
// 状态码语义：
//
//	405 — 非 POST
//	403 — 认证失败（凭据未配置 / 验签失败 / 解密失败）
//	400 — 已通过验签，但请求体畸形（消息事件缺必需字段）
//	200 — 验签通过。消息事件已交下游；非消息事件无需处理
//	500 — 下游处理失败（让飞书重试，避免消息永久丢失）
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 凭据未配置时拒绝一切请求，**包括 URL 挑战**。
	//
	// 这是对参照实现的一处有意偏离，必须明说：WeKnora 的 HandleURLVerification
	// （internal/im/feishu/adapter.go:238-283）只解密不校验凭据，未配置 token 的
	// 部署仍会应答挑战。而 issue #5 的 AC 明确要求「未配置 verification token 时，
	// 端点拒绝所有回调（fail-closed），不是放行」——把挑战请求排除在该约束外，
	// 等于承认「凭据缺失时端点仍对外服务」。
	//
	// 代价：未配置凭据的部署无法完成飞书首次 URL 校验，会看到 403 而不是
	// challenge 回显。这是期望行为——配置凭据是启用端点的前置条件，
	// 不是可选的调优项。
	if h.cfg.VerificationToken == "" {
		h.logf("feishu webhook: rejected (verification token not configured)")
		forbidden(w, "webhook not configured")
		return
	}

	// 挑战请求：已配置凭据前提下应答，不比对 token 值（见 HandleURLVerification 注释）
	if h.source.HandleURLVerification(w, r) {
		h.logf("feishu webhook: answered URL verification challenge")
		return
	}

	if err := h.source.VerifyCallback(r); err != nil {
		// 不回显 err 详情：错误可能含内部结构信息，而调用方是未认证的。
		h.logf("feishu webhook: verification failed: %v", err)
		forbidden(w, "verification failed")
		return
	}

	msg, err := h.source.ParseCallback(r)
	if err != nil {
		// 验签已通过 —— 这是已认证但畸形的请求，语义上是 400 而非 403。
		h.logf("feishu webhook: parse failed: %v", err)
		http.Error(w, "malformed callback", http.StatusBadRequest)
		return
	}
	if msg == nil {
		// 非消息事件（如会话被解散、表情回复）——验签有效，无事可做。
		h.logf("feishu webhook: non-message event ignored")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Demo path 的证据面：日志打印解析后的发送者 open_id、chat_id、消息文本。
	// 这里只打印解析结果，不打印原始 body——原始 body 可能含凭据字段。
	h.logf("feishu inbound: open_id=%s chat_id=%s chat_type=%s message_id=%s text=%q",
		msg.UserID, msg.ChatID, msg.ChatType, msg.MessageID, msg.Content)

	if h.onMessage != nil {
		if err := h.onMessage(msg); err != nil {
			h.logf("feishu webhook: downstream handler failed: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// forbidden 写出 403，body 为固定文案——不回显任何请求内容。
func forbidden(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
}
