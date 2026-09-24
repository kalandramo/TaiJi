package channel

import (
	"context"
	"testing"
)

// 流式卡片能力契约（issue #10）。
//
// 为什么用**可选接口**而非扩展 Sender：
//   - 不是所有渠道都有卡片能力（如纯文本渠道）
//   - 扩展 Sender 会迫使所有实现提供空方法（正是本包注释批评的形态）
//   - 管道用类型断言探测能力，不支持时自动降级到纯文本
//
// 分层：定义在 channel（契约层）而非 server（编排层），
// 因为 server 不依赖具体渠道实现（实测确认），而卡片是渠道能力。

// 编译期断言：fakeStreamingSender 满足 StreamingSender。
type fakeStreamingSender struct {
	created   int
	streamed  []string
	closed    bool
	sendErr   error
	createErr error
}

func (f *fakeStreamingSender) SendMessage(ctx context.Context, to, text string, opts SendOptions) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "om_1", nil
}

func (f *fakeStreamingSender) StartCardStream(ctx context.Context, to string, opts SendOptions) (CardStream, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created++
	return &fakeCardStream{parent: f}, nil
}

type fakeCardStream struct {
	parent *fakeStreamingSender
}

func (c *fakeCardStream) Update(ctx context.Context, text string) error {
	c.parent.streamed = append(c.parent.streamed, text)
	return nil
}

func (c *fakeCardStream) Close(ctx context.Context, finalText string) error {
	c.parent.streamed = append(c.parent.streamed, finalText)
	c.parent.closed = true
	return nil
}

var _ StreamingSender = (*fakeStreamingSender)(nil)

func TestStreamingSender_InterfaceShape(t *testing.T) {
	// 契约：StartCardStream 返回可 Update/Close 的流。
	var s StreamingSender = &fakeStreamingSender{}
	stream, err := s.StartCardStream(context.Background(), "ou_user", SendOptions{})
	if err != nil {
		t.Fatalf("StartCardStream: %v", err)
	}
	if stream == nil {
		t.Fatal("应返回非 nil 的 CardStream")
	}
	if err := stream.Update(context.Background(), "第一块"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := stream.Close(context.Background(), "最终内容"); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// 能力探测：非流式 Sender 不应被误判为 StreamingSender。
func TestStreamingSender_CapabilityDetection(t *testing.T) {
	type plainSender struct{ Sender }
	var s Sender = plainSender{}

	if _, ok := s.(StreamingSender); ok {
		t.Error("纯文本 Sender 不应被判定为支持流式卡片")
	}
}

// 支持流式的 Sender 应被正确探测。
func TestStreamingSender_DetectionPositive(t *testing.T) {
	var s Sender = &fakeStreamingSender{}
	if _, ok := s.(StreamingSender); !ok {
		t.Error("支持流式的 Sender 应被探测到")
	}
}
