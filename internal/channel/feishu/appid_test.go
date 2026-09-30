package feishu

import (
	"testing"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/kalandramo/TaiJi/internal/channel"
)

// 多 bot 分流：入站消息必须携带「接收方应用 ID」（形态 C 的核心）。
//
// 背景：TaiJi 原实现只读 ev.Event.*，丢弃了事件头的 Header.AppID——
// 而多 bot 部署下，不同 agent 绑不同飞书应用，必须据此判定消息归属。
//
// 依赖事实（已核验依赖源码 service/im/v1/model.go:15219-15223）：
//
//	type P2MessageReceiveV1 struct {
//		*larkevent.EventV2Base   // ← 嵌入指针，含 Header.AppID
//		*larkevent.EventReq
//		Event *P2MessageReceiveV1Data
//	}
//
// 注意 EventV2Base 是**嵌入指针**，可能为 nil——实现必须做两层判空。

func TestIncomingFromLongConnEvent_CarriesAppID(t *testing.T) {
	str := func(s string) *string { return &s }
	ev := &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{AppID: "cli_aaa"},
		},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: str("ou_sender")},
			},
			Message: &larkim.EventMessage{
				MessageId: str("om_1"),
				ChatId:    str("oc_group"),
				ChatType:  str("group"),
				Content:   str(`{"text":"你好"}`),
			},
		},
	}

	msg := incomingFromLongConnEvent(ev)
	if msg == nil {
		t.Fatal("message must be produced")
	}
	if msg.AppID != "cli_aaa" {
		t.Errorf("AppID = %q, want cli_aaa——多 bot 分流据此判定，"+
			"丢失会导致所有 bot 的消息无法区分", msg.AppID)
	}
}

// EventV2Base 为 nil 时不得 panic（嵌入指针的真实风险）。
func TestIncomingFromLongConnEvent_NilEventV2Base(t *testing.T) {
	str := func(s string) *string { return &s }
	ev := &larkim.P2MessageReceiveV1{
		// EventV2Base 故意为 nil
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: str("ou_sender")},
			},
			Message: &larkim.EventMessage{
				MessageId: str("om_1"),
				ChatId:    str("oc_g"),
				ChatType:  str("group"),
				Content:   str(`{"text":"hi"}`),
			},
		},
	}

	msg := incomingFromLongConnEvent(ev) // 不得 panic
	if msg == nil {
		t.Fatal("无事件头时仍应产出消息（AppID 为空）")
	}
	if msg.AppID != "" {
		t.Errorf("无事件头时 AppID 应为空，got %q", msg.AppID)
	}
}

// Header 为 nil 时不得 panic（EventV2Base 非 nil 但 Header 为 nil）。
func TestIncomingFromLongConnEvent_NilHeader(t *testing.T) {
	str := func(s string) *string { return &s }
	ev := &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{Header: nil},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: str("ou_sender")},
			},
			Message: &larkim.EventMessage{
				MessageId: str("om_1"),
				ChatId:    str("oc_g"),
				ChatType:  str("group"),
				Content:   str(`{"text":"hi"}`),
			},
		},
	}

	msg := incomingFromLongConnEvent(ev) // 不得 panic
	if msg == nil {
		t.Fatal("Header 为 nil 时仍应产出消息")
	}
	if msg.AppID != "" {
		t.Errorf("Header 为 nil 时 AppID 应为空，got %q", msg.AppID)
	}
}

// 契约完整性：AppID 字段存在于 IncomingMessage。
func TestIncomingMessage_HasAppIDField(t *testing.T) {
	m := channel.IncomingMessage{AppID: "cli_x"}
	if m.AppID != "cli_x" {
		t.Error("IncomingMessage.AppID 字段缺失或不可赋值")
	}
}
