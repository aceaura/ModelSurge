package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// Chat 的错误可以在流中途以 {"error":{...}} 单独成帧到达：没有 [DONE]，也没有
// choices。此前 response 结构体没有 error 字段，整帧被当成一个空 chunk 解析，
// 于是 relay 认为请求成功（没有 EvError 就不重试、不换账号），客户端只看到一个
// 空回答——错误详情一个字都不剩。
const (
	efChunk = `{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"content":"前半"}}]}`
	efCtx   = `{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error","param":null,"code":"context_length_exceeded"}}`
	efRate  = `{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`
)

// efFeed 喂一串帧并收齐 IR 事件（含 Finish）。
func efFeed(t *testing.T, datas ...string) []ir.Event {
	t.Helper()
	dec := codec{}.NewStreamDecoder()
	var evs []ir.Event
	for _, d := range datas {
		got, err := dec.Feed("", d)
		if err != nil {
			t.Fatalf("Feed(%s): %v", d, err)
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

func TestStreamDecodeInStreamErrorSurfacesDetails(t *testing.T) {
	e := efErr(efFeed(t, efCtx))
	if e == nil {
		t.Fatal("流内错误帧没有产出 EvError")
	}
	if e.Type != "invalid_request_error" || e.Code != "context_length_exceeded" ||
		!strings.Contains(e.Message, "maximum context length") {
		t.Fatalf("错误详情丢失：type=%q code=%q msg=%q", e.Type, e.Code, e.Message)
	}
}

// 错误帧是终止帧：不得再补 message_delta+message_stop，否则客户端在错误之后
// 又看到一个正常收尾。
func TestStreamDecodeInStreamErrorIsTerminal(t *testing.T) {
	evs := efFeed(t, efChunk, efCtx)
	if efCount(evs, ir.EvMessageDelta) != 0 {
		t.Errorf("错误之后不得有 message_delta")
	}
	if efCount(evs, ir.EvMessageStop) != 0 {
		t.Errorf("错误之后不得有 message_stop")
	}
}

// 首帧即错误时不得凭空开一个 message：那会让客户端以为收到了一次空补全。
func TestStreamDecodeErrorFirstFrameDoesNotStartMessage(t *testing.T) {
	evs := efFeed(t, efCtx)
	if efCount(evs, ir.EvMessageStart) != 0 {
		t.Errorf("纯错误流不得产出 message_start")
	}
	if efCount(evs, ir.EvError) != 1 {
		t.Errorf("期望恰好 1 个 EvError，实得 %d", efCount(evs, ir.EvError))
	}
}

// 错误之前已开的块仍要关掉，不给下游留一个永不结束的块。
func TestStreamDecodeErrorAfterDeltaStillClosesBlocks(t *testing.T) {
	evs := efFeed(t, efChunk, efCtx)
	if efCount(evs, ir.EvBlockStop) == 0 {
		t.Errorf("错误后未关闭已开的块")
	}
}

// 上下文超长换谁都会被同样拒绝，判成可重试只会白烧账号池。
func TestStreamDecodeContextLengthNotRetryable(t *testing.T) {
	e := efErr(efFeed(t, efCtx))
	if e == nil || e.Retryable {
		t.Fatalf("invalid_request_error 必须不可重试：%+v", e)
	}
}

// 限流则相反：换一个账号/目标就可能成功。
func TestStreamDecodeRateLimitRetryable(t *testing.T) {
	e := efErr(efFeed(t, efRate))
	if e == nil {
		t.Fatal("没有产出 EvError")
	}
	if !e.Retryable {
		t.Errorf("rate_limit_error 应可重试")
	}
	if e.Code != "rate_limit_exceeded" {
		t.Errorf("错误码丢失：code=%q", e.Code)
	}
}

// 错误帧之后上游仍可能发 [DONE]，不得因此又补一轮收尾事件。
func TestStreamDecodeErrorThenDoneStaysError(t *testing.T) {
	evs := efFeed(t, efCtx, "[DONE]")
	if efCount(evs, ir.EvError) != 1 {
		t.Errorf("期望 1 个 EvError，实得 %d", efCount(evs, ir.EvError))
	}
	if efCount(evs, ir.EvMessageDelta) != 0 || efCount(evs, ir.EvMessageStop) != 0 {
		t.Errorf("[DONE] 之后不得补收尾事件")
	}
}

// 类型缺席时保留规范默认值，不得覆盖成空串。
func TestStreamDecodeErrorWithoutTypeKeepsCanonical(t *testing.T) {
	e := efErr(efFeed(t, `{"error":{"message":"boom"}}`))
	if e == nil {
		t.Fatal("没有产出 EvError")
	}
	if e.Type != ir.ErrTypeConnection {
		t.Errorf("应回落 %q，实得 %q", ir.ErrTypeConnection, e.Type)
	}
	if e.Message != "boom" {
		t.Errorf("已有的 message 不得丢：msg=%q", e.Message)
	}
}

// 错误体在但 message 缺席时保留兜底文案，不得覆盖成空串。
func TestStreamDecodeErrorWithoutMessageKeepsFallback(t *testing.T) {
	e := efErr(efFeed(t, `{"error":{"type":"invalid_request_error","code":"c1"}}`))
	if e == nil {
		t.Fatal("没有产出 EvError")
	}
	if e.Message == "" {
		t.Errorf("消息缺席时不得覆盖成空串")
	}
	if e.Code != "c1" || e.Type != "invalid_request_error" {
		t.Errorf("已有字段不得丢：type=%q code=%q", e.Type, e.Code)
	}
}

// 决定性回归：编码器写出的错误帧，同族解码器必须读得回来。此前 RenderStreamError
// 产出 {"error":{...}}，而解码器不认这个字段，chat->chat 往返把错误整个吃掉。
func TestChatRoundTripErrorFrameDecodable(t *testing.T) {
	frame := string(codec{}.RenderStreamError(&ir.Error{
		Type: ir.ErrTypeContentFilter, Code: "cyber_policy", Message: "blocked",
	}))
	payload := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(frame, "data: ", 2)[1], ""))
	payload = strings.SplitN(payload, "\n\n", 2)[0]
	e := efErr(efFeed(t, payload))
	if e == nil {
		t.Fatalf("本族 RenderStreamError 的产物解不出错误：%s", frame)
	}
	if e.Code != "cyber_policy" || !strings.Contains(e.Message, "blocked") {
		t.Errorf("往返丢详情：type=%q code=%q msg=%q", e.Type, e.Code, e.Message)
	}
}

// 正常流不受影响（对照）。
func TestStreamDecodeNormalStreamUnaffected(t *testing.T) {
	evs := efFeed(t, efChunk,
		`{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
	if efCount(evs, ir.EvError) != 0 {
		t.Errorf("正常流不得产出 EvError")
	}
	if efCount(evs, ir.EvMessageDelta) != 1 || efCount(evs, ir.EvMessageStop) != 1 {
		t.Errorf("正常流必须收尾：%+v", evs)
	}
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta && ev.StopReason != ir.StopEndTurn {
			t.Errorf("停止原因 = %q, want %q", ev.StopReason, ir.StopEndTurn)
		}
	}
}
