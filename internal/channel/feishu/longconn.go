package feishu

import (
	"context"
	"errors"
	"fmt"
	"sync"

	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 飞书长连接（WebSocket）接入（issue #9 Wave 5，AC-6）。
//
// 与 webhook 的分工（设计文档 §2.2）：
//   - webhook：被推送。需要公网可达 URL，需要验签（渠道层唯一信任边界）
//   - 长连接：主动拉。只需出网，无需公网入口，**无需验签**
//     （信任来自 SDK 与飞书之间的 TLS 通道）
//
// 两种形态的入站方向不同，这正是 §4.4.1 把渠道接口拆两层的原因。

// wsClient 是 larkws.Client 的最小方法面。
//
// 抽成接口是为了**可测试性**：AC-6 要验证「退出时长连接被真正关闭」，
// 而这需要观察 Start 的阻塞与 Close 的调用——用真实 WebSocket 无法
// 在单测里做到（需要真实飞书端点）。
type wsClient interface {
	// Start 建立连接并阻塞。
	//
	// **重要（源码级确认）**：larkws.Client.Start 的末尾是裸 `select{}`
	// （ws/client.go:206-232），它**不观察 ctx**——取消 ctx 不会让它返回，
	// socket 会存活并自动重连。所以必须在 goroutine 中调用它，
	// 并用 Close 停止。
	Start(ctx context.Context) error
	// Close 真正断开连接（ws/client.go:177-180：设 autoReconnect=false
	// 再 disconnect）。这是**唯一**的停止路径。
	Close()
}

// larkWSAdapter 把 *larkws.Client 适配到 wsClient。
type larkWSAdapter struct{ c *larkws.Client }

func (a larkWSAdapter) Start(ctx context.Context) error { return a.c.Start(ctx) }
func (a larkWSAdapter) Close()                          { a.c.Close() }

// LongConnConfig 是长连接的装配参数。
type LongConnConfig struct {
	AppID     string
	AppSecret string

	// OnMessage 是解析出消息后的下游回调（通常是 dispatcher.Enqueue）。
	OnMessage func(*channel.IncomingMessage) error

	// Logf 是日志出口。
	Logf func(format string, args ...any)

	// newClient 是测试接缝：注入 fake wsClient。nil 则用真实 SDK。
	newClient func(appID, appSecret string, handler *larkdispatcher.EventDispatcher) wsClient
}

// LongConn 是长连接的生命周期持有者。
type LongConn struct {
	cfg    LongConnConfig
	client wsClient

	mu      sync.Mutex
	started bool
	stopped bool
	// done 在 Start 的 goroutine 退出时关闭，供 Stop 等待。
	done chan struct{}
	// startErr 保存 Start 的返回值（在 Stop 之后可读）。
	startErr error
}

// NewLongConn 装配长连接客户端。
//
// 凭据缺失即报错——长连接需要 appID/appSecret 建立 WS 并换取 token。
func NewLongConn(cfg LongConnConfig) (*LongConn, error) {
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf(
			"feishu: long connection requires %s and %s", EnvAppID, EnvAppSecret)
	}
	if cfg.newClient == nil {
		cfg.newClient = func(appID, appSecret string, handler *larkdispatcher.EventDispatcher) wsClient {
			return larkWSAdapter{c: larkws.NewClient(appID, appSecret,
				larkws.WithEventHandler(handler))}
		}
	}

	// 事件分发器：长连接模式下 verificationToken/encryptKey 传空串——
	// 源码注释明说长连接不需要它们（WeKnora longconn.go:31-32）。
	d := larkdispatcher.NewEventDispatcher("", "")
	d.OnP2MessageReceiveV1(func(ctx context.Context, ev *larkim.P2MessageReceiveV1) error {
		msg := incomingFromLongConnEvent(ev)
		if msg == nil {
			return nil // 非消息事件或字段残缺，静默跳过
		}
		if cfg.OnMessage != nil {
			return cfg.OnMessage(msg)
		}
		return nil
	})

	return &LongConn{
		cfg:    cfg,
		client: cfg.newClient(cfg.AppID, cfg.AppSecret, d),
		done:   make(chan struct{}),
	}, nil
}

// Start 建立连接并在**后台**运行。
//
// 必须异步：Start 阻塞在裸 select{}（见 wsClient 的注释），
// 同步调用会让调用方永远卡住。返回 nil 表示 goroutine 已拉起，
// 不代表连接已建立（连接状态由 SDK 的 onReady/onError 回调反映）。
func (l *LongConn) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return errors.New("feishu: long connection already stopped")
	}
	if l.started {
		l.mu.Unlock()
		return errors.New("feishu: long connection already started")
	}
	l.started = true
	l.mu.Unlock()

	go func() {
		defer close(l.done)
		err := l.client.Start(ctx)
		l.mu.Lock()
		l.startErr = err
		l.mu.Unlock()
		if err != nil && l.cfg.Logf != nil {
			l.cfg.Logf("feishu longconn: start returned error: %v", err)
		}
	}()
	return nil
}

// Stop 真正关闭连接，并等待 Start 的 goroutine 退出。
//
// 这是 AC-6 的核心：**必须调用 client.Close()**，而不是只取消 ctx。
// 源码依据：Start 末尾是裸 select{}，不观察 ctx；Close 才会
// 设 autoReconnect=false 并 disconnect（ws/client.go:177-180）。
// 只取消 ctx 会让 socket 存活并自动重连——正是 AC-6 要防的
// 「留下存活 socket」。
//
// 幂等：重复调用安全。
func (l *LongConn) Stop() {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	l.stopped = true
	started := l.started
	l.mu.Unlock()

	// 先关闭连接——这会解除 Start 的阻塞（disconnect 让 select{} 退出？
	// 实际上 select{} 永不退出，但 SDK 的 disconnect 会关闭底层 socket，
	// 这正是 AC-6 要的「不留存活 socket」）。
	l.client.Close()

	// 若 Start 已拉起，等它的 goroutine 结束，避免测试中出现
	// 「Stop 返回后 goroutine 仍在跑」的竞态。
	if started {
		<-l.done
	}
}

// StartErr 返回 Start 的返回值（Stop 之后才有意义）。
func (l *LongConn) StartErr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.startErr
}

// incomingFromLongConnEvent 把 SDK 的长连接事件转成统一消息。
//
// 与 parse.go 的 ParseCallback 是**两条入站路径**（长连接 vs webhook），
// 但产出同一个 channel.IncomingMessage——这是 §4.4.1 分层的目的：
// 下游（门禁/路由/执行/出站）不关心消息从哪来。
//
// 字段映射与 parse.go 保持一致（主体取 open_id、chat_type 归一化、
// thread_id 决定话题），避免两条路径的语义漂移。
func incomingFromLongConnEvent(ev *larkim.P2MessageReceiveV1) *channel.IncomingMessage {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil {
		return nil
	}
	m := ev.Event.Message

	// 主体 ID：只认 open_id（与 parse.go 的 extractOpenID 同规则）。
	var openID string
	if ev.Event.Sender != nil && ev.Event.Sender.SenderId != nil && ev.Event.Sender.SenderId.OpenId != nil {
		openID = *ev.Event.Sender.SenderId.OpenId
	}
	if openID == "" {
		// 无主体 ID 的消息无法参与权限判定——丢弃而非产出残缺消息。
		// 与 parse.go 的 fail-closed 取向一致。
		return nil
	}

	var chatID, chatType, messageID, content, threadID, rootID string
	if m.ChatId != nil {
		chatID = *m.ChatId
	}
	if m.ChatType != nil {
		chatType = *m.ChatType
	}
	if m.MessageId != nil {
		messageID = *m.MessageId
	}
	if m.Content != nil {
		content = *m.Content
	}
	if m.ThreadId != nil {
		threadID = *m.ThreadId
	}
	if m.RootId != nil {
		rootID = *m.RootId
	}

	text := extractText(content)
	mentions := extractLongConnMentions(m)

	return &channel.IncomingMessage{
		Platform:  channel.PlatformFeishu,
		UserID:    openID,
		ChatID:    chatID,
		ChatType:  normalizeChatType(chatType),
		MessageID: messageID,
		Content:   text,
		Mentions:  mentions,
		Meta: &channel.ChannelMessageMeta{
			Provider:          string(channel.PlatformFeishu),
			ChatType:          chatType,
			NativeContextType: nativeContextType(threadID),
			ThreadID:          threadID,
			RootID:            rootID,
			MessageID:         messageID,
			Text:              text,
		},
	}
}

// extractLongConnMentions 从 SDK 事件取 @ 列表。
//
// 与 parse.go 的 extractMentions 同规则：只保留有 open_id 的条目，
// 丢弃无法参与身份比对的空条目。
func extractLongConnMentions(m *larkim.EventMessage) []channel.Mention {
	if m == nil || len(m.Mentions) == 0 {
		return nil
	}
	out := make([]channel.Mention, 0, len(m.Mentions))
	for _, mm := range m.Mentions {
		if mm == nil || mm.Id == nil || mm.Id.OpenId == nil || *mm.Id.OpenId == "" {
			continue
		}
		var key, name string
		if mm.Key != nil {
			key = *mm.Key
		}
		if mm.Name != nil {
			name = *mm.Name
		}
		out = append(out, channel.Mention{OpenID: *mm.Id.OpenId, Key: key, Name: name})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
