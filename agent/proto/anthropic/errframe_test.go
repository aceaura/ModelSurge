package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

const (
	efStart = `{"type":"message_start","message":{"id":"m1","model":"claude"}}`
	efBlk   = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	efDelta = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"前半"}}`
	efStop  = `{"type":"message_stop"}`
)

// efFeed 喂一串 (event, data) 并收齐 IR 事件（含 Finish）。
func efFeed(t *testing.T, pairs ...string) []ir.Event {
	t.Helper()
	dec := New().NewStreamDecoder()
	var evs []ir.Event
	for i := 0; i+1 < len(pairs); i += 2 {
		got, err := dec.Feed(pairs[i], pairs[i+1])
		if err != nil {
			t.Fatalf("Feed(%s): %v", pairs[i], err)
		}
		evs = append(evs, got...)
	}
	return append(evs, dec.Finish()...)
}

func efCount(evs []ir.Event, t ir.EventType) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == t {
			n++
		}
	}
	return n
}

func efErr(evs []ir.Event) *ir.Error {
	for _, ev := range evs {
		if ev.Type == ir.EvError {
			return ev.Err
		}
	}
	return nil
}

func efErrFrame(typ, msg string) string {
	return `{"type":"error","error":{"type":"` + typ + `","message":"` + msg + `"}}`
}

// 类型缺席时不得把规范默认值覆盖成空串：type 为空会让下游 RenderStreamError
// 写出 "type":""，客户端无从判断该不该重试。
func TestStreamDecodeErrorWithoutTypeKeepsCanonical(t *testing.T) {
	e := efErr(efFeed(t, "message_start", efStart, "error", `{"type":"error","error":{"message":"boom"}}`))
	if e == nil {
		t.Fatal("没有产出 EvError")
	}
	if e.Type != ir.ErrTypeUpstream {
		t.Errorf("应回落 %q，实得 %q", ir.ErrTypeUpstream, e.Type)
	}
	if e.Message != "boom" {
		t.Errorf("已有的 message 不得丢：msg=%q", e.Message)
	}
}

// 错误体整个缺席时同样要有规范类型与非空消息。
func TestStreamDecodeErrorWithoutBodyKeepsCanonical(t *testing.T) {
	e := efErr(efFeed(t, "message_start", efStart, "error", `{"type":"error"}`))
	if e == nil {
		t.Fatal("没有产出 EvError")
	}
	if e.Type != ir.ErrTypeUpstream || e.Message == "" {
		t.Errorf("应回落规范类型与非空消息：type=%q msg=%q", e.Type, e.Message)
	}
}

// 错误体存在但 message 缺席——这一格与「错误体整个缺席」不同：它会走进覆盖分支，
// 空串不得把回落文案冲掉，否则 RenderStreamError 写出 "message":""，客户端拿到
// 一个没有任何可读原因的错误。
func TestStreamDecodeErrorWithoutMessageKeepsFallback(t *testing.T) {
	e := efErr(efFeed(t, "message_start", efStart, "error",
		`{"type":"error","error":{"type":"overloaded_error"}}`))
	if e == nil {
		t.Fatal("没有产出 EvError")
	}
	if e.Message == "" {
		t.Error("message 缺席时必须保留回落文案，实得空串")
	}
	if e.Type != ir.ErrTypeOverloaded {
		t.Errorf("已有的 type 不得丢：type=%q", e.Type)
	}
	if !e.Retryable {
		t.Error("overloaded 应可重试")
	}
}

// error 是终止事件：Finish() 不得再补 message_delta{aborted}+message_stop。
func TestStreamDecodeErrorIsTerminal(t *testing.T) {
	evs := efFeed(t, "message_start", efStart, "content_block_start", efBlk,
		"content_block_delta", efDelta, "error", efErrFrame("overloaded_error", "Overloaded"))
	if efCount(evs, ir.EvMessageDelta) != 0 {
		t.Errorf("错误之后不得有 message_delta")
	}
	if efCount(evs, ir.EvMessageStop) != 0 {
		t.Errorf("错误之后不得有 message_stop")
	}
	if efCount(evs, ir.EvError) != 1 {
		t.Errorf("期望恰好 1 个 EvError，实得 %d", efCount(evs, ir.EvError))
	}
}

// 错误之后上游仍发来 message_stop 时不得透传：那会产出 stop->delta 的颠倒序列
// （message_stop 先到、Finish 的 message_delta 后到），下游编码器收到畸形形状。
func TestStreamDecodeMessageStopAfterErrorIgnored(t *testing.T) {
	evs := efFeed(t, "message_start", efStart, "content_block_start", efBlk,
		"content_block_delta", efDelta,
		"error", efErrFrame("overloaded_error", "Overloaded"),
		"message_stop", efStop)
	if efCount(evs, ir.EvMessageStop) != 0 {
		t.Errorf("错误之后的 message_stop 应被忽略")
	}
	if efCount(evs, ir.EvMessageDelta) != 0 {
		t.Errorf("错误之后不得有 message_delta")
	}
}

// 非法请求换谁都会被同样拒绝，判成可重试只会白烧账号池。
func TestStreamDecodeInvalidRequestNotRetryable(t *testing.T) {
	e := efErr(efFeed(t, "message_start", efStart,
		"error", efErrFrame("invalid_request_error", "max_tokens: field required")))
	if e == nil || e.Retryable {
		t.Fatalf("invalid_request_error 必须不可重试：%+v", e)
	}
}

// 认证失败要可重试：换一个账号可能就成了（对齐 ClassifyStatus 的 401 口径）。
func TestStreamDecodeAuthErrorRetryable(t *testing.T) {
	e := efErr(efFeed(t, "message_start", efStart,
		"error", efErrFrame("authentication_error", "invalid x-api-key")))
	if e == nil {
		t.Fatal("没有产出 EvError")
	}
	if !e.Retryable {
		t.Errorf("authentication_error 应可重试（换账号）")
	}
	if e.Type != ir.ErrTypeAuth {
		t.Errorf("类型应保真为 %q，实得 %q", ir.ErrTypeAuth, e.Type)
	}
}

func TestStreamDecodeOverloadedRetryable(t *testing.T) {
	e := efErr(efFeed(t, "message_start", efStart,
		"error", efErrFrame("overloaded_error", "Overloaded")))
	if e == nil || !e.Retryable {
		t.Fatalf("overloaded_error 应可重试：%+v", e)
	}
	if e.Message != "Overloaded" {
		t.Errorf("消息丢失：msg=%q", e.Message)
	}
}

// 决定性回归：编码器写出的错误帧，同族解码器必须读得回来。
func TestAnthropicRoundTripErrorFrameDecodable(t *testing.T) {
	frame := string(codec{}.RenderStreamError(&ir.Error{
		Type: ir.ErrTypeContentFilter, Code: "cyber_policy", Message: "blocked",
	}))
	var data string
	for _, l := range strings.Split(frame, "\n") {
		if strings.HasPrefix(l, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(l, "data:"))
		}
	}
	if data == "" {
		t.Fatalf("RenderStreamError 没有 data 行：%q", frame)
	}
	e := efErr(efFeed(t, "error", data))
	if e == nil {
		t.Fatalf("本族 RenderStreamError 的产物解不出错误：%s", frame)
	}
	if !strings.Contains(e.Message, "blocked") {
		t.Errorf("往返丢详情：type=%q msg=%q", e.Type, e.Message)
	}
}

// 正常流不受影响（对照）：message_stop 照常透传，Finish 不再补收尾。
func TestStreamDecodeNormalStopUnaffected(t *testing.T) {
	evs := efFeed(t, "message_start", efStart, "content_block_start", efBlk,
		"content_block_delta", efDelta,
		"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		"message_stop", efStop)
	if efCount(evs, ir.EvError) != 0 {
		t.Errorf("正常流不得产出 EvError")
	}
	if efCount(evs, ir.EvMessageDelta) != 1 || efCount(evs, ir.EvMessageStop) != 1 {
		t.Errorf("正常流必须恰好收尾一次：%+v", evs)
	}
}
