package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R104：怪异上游 200 返回 {"error":{...}} 时必须报错，不得产出 choices 为空的
// 伪造成功补全（new-api 非流式路径同样先查顶层 error 再解析）。
func TestDecodeResponseTopLevelErrorBody(t *testing.T) {
	_, _, err := (codec{}).DecodeResponseWithNotes([]byte(
		`{"error":{"type":"rate_limit_error","message":"slow down","code":"rl_1"}}`))
	if err == nil {
		t.Fatal("200+error 体被伪造成成功响应")
	}
	ie, ok := err.(*ir.Error)
	if !ok {
		t.Fatalf("错误类型 = %T，want *ir.Error", err)
	}
	if ie.Type != ir.ErrTypeRateLimit || ie.Message != "slow down" || ie.Code != "rl_1" {
		t.Errorf("错误体没透传：%+v", ie)
	}
	if !ie.Retryable {
		t.Error("rate_limit 应可重试")
	}
}

// 正常响应不受顶层 error 检查影响。
func TestDecodeResponseWithoutErrorUnaffected(t *testing.T) {
	resp, _, err := (codec{}).DecodeResponseWithNotes([]byte(
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatalf("正常响应被误判：%v", err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("正文丢了")
	}
}

// R104：扁平工具形态 {name, parameters} 的 schema 不得静默变 nil——
// input_schema 与 parameters 都是声明过的接收键，谁有值用谁。
func TestDecodeFlatToolParametersCoalesced(t *testing.T) {
	req, err := (codec{}).DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || len(req.Tools[0].InputSchema) == 0 {
		t.Fatalf("扁平 parameters 丢了：%+v", req.Tools)
	}
	out, err := (codec{}).EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"city"`) {
		t.Errorf("出站工具 schema 空了：%s", out)
	}
}

// input_schema 优先于 parameters（两个键都给时不得串味）。
func TestDecodeFlatToolInputSchemaWins(t *testing.T) {
	req, err := (codec{}).DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"t","input_schema":{"type":"object","description":"A"},"parameters":{"type":"object","description":"B"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(req.Tools[0].InputSchema), `"A"`) {
		t.Fatalf("input_schema 没优先：%s", req.Tools[0].InputSchema)
	}
}

// R104：store / prompt_cache_retention 同族往返（三态 store 显式 false 也得留住）。
func TestStoreAndRetentionRoundTrip(t *testing.T) {
	req, err := (codec{}).DecodeRequest([]byte(`{"model":"m","store":false,"prompt_cache_retention":"24h",` +
		`"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Store == nil || *req.Store {
		t.Fatalf("显式 store=false 没落 IR：%+v", req.Store)
	}
	if req.PromptCacheRetention != "24h" {
		t.Fatalf("retention 没落 IR：%q", req.PromptCacheRetention)
	}
	out, err := (codec{}).EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"store":false`) || !strings.Contains(s, `"prompt_cache_retention":"24h"`) {
		t.Errorf("出站 store/retention 丢了：%s", s)
	}
}

// R104 流式 created 保真：chunk.created 随 EvMessageStart 交付，编码器原值回写。
func TestStreamCreatedRoundTrip(t *testing.T) {
	dec := (codec{}).NewStreamDecoder()
	evs, err := dec.Feed("data", `{"id":"c1","created":1700000000,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var start *ir.Event
	for i := range evs {
		if evs[i].Type == ir.EvMessageStart {
			start = &evs[i]
		}
	}
	if start == nil || start.Created != 1700000000 {
		t.Fatalf("EvMessageStart.Created = %+v，want 1700000000", evs)
	}
	enc := (codec{}).NewStreamEncoder()
	frames, err := enc.Encode(*start)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, fr := range frames {
		joined += string(fr)
	}
	if !strings.Contains(joined, `"created":1700000000`) {
		t.Errorf("出站 created 被本地钟覆盖：%s", joined)
	}
}

// R104：零增量工具调用关块时补 "{}"——客户端只从 delta 拼参数，一个 delta
// 都不发就拼出 ""，json.loads 直接崩（无参工具是常态）。
func TestStreamEncoderZeroArgToolCallEmitsEmptyObject(t *testing.T) {
	enc := (codec{}).NewStreamEncoder()
	must := func(ev ir.Event) [][]byte {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode %v: %v", ev.Type, err)
		}
		return frames
	}
	must(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"})
	must(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
		ID: "call_1", Name: "ping",
	}}})
	frames := must(ir.Event{Type: ir.EvBlockStop, Index: 0})
	joined := ""
	for _, fr := range frames {
		joined += string(fr)
	}
	if !strings.Contains(joined, `"arguments":"{}"`) {
		t.Fatalf("零增量工具调用没补 {}：%s", joined)
	}
}

// R104 终止守卫：EvError 自带 [DONE] 且置 stopped，之后的 message_delta /
// message_stop 一律不得再发（否则限流被读成「输出超长」+ 第二个 [DONE]）。
func TestStreamEncoderSuppressesFramesAfterError(t *testing.T) {
	enc := (codec{}).NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	errFrames, err := enc.Encode(ir.Event{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeOverloaded, Message: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(join(errFrames)), "[DONE]") != 1 {
		t.Fatalf("错误帧该自带一个 [DONE]：%q", errFrames)
	}
	if frames, _ := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn}); len(frames) != 0 {
		t.Errorf("错误之后又发出 message_delta：%q", frames)
	}
	if frames, _ := enc.Encode(ir.Event{Type: ir.EvMessageStop}); len(frames) != 0 {
		t.Errorf("错误之后又发出第二个 [DONE]：%q", frames)
	}
}

func join(frames [][]byte) []byte {
	var out []byte
	for _, fr := range frames {
		out = append(out, fr...)
	}
	return out
}
