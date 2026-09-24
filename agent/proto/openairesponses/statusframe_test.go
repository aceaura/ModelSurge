package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R104：非流式响应的终态失败不得伪造成 completed——此前只判 incomplete，
// failed/cancelled 会产出 200+空 output 的"成功"，error 全丢，与流式路径口径相反。

func TestDecodeResponseFailedReturnsError(t *testing.T) {
	_, err := New().DecodeResponse([]byte(`{"id":"r1","status":"failed",` +
		`"error":{"code":"server_error","message":"boom"},"output":[]}`))
	if err == nil {
		t.Fatal("status=failed 被伪造成成功响应")
	}
	ie, ok := err.(*ir.Error)
	if !ok {
		t.Fatalf("错误类型 = %T，want *ir.Error", err)
	}
	if ie.Message != "boom" || ie.Code != "server_error" {
		t.Errorf("错误体没透传：%+v", ie)
	}
}

func TestDecodeResponseCancelledReturnsError(t *testing.T) {
	_, err := New().DecodeResponse([]byte(`{"id":"r1","status":"cancelled","output":[]}`))
	if err == nil {
		t.Fatal("status=cancelled 被伪造成成功响应")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("兜底信息没带状态：%v", err)
	}
}

// background 模式的非终态：换一个真流式的目标可能成，判可重试的传输族错误。
func TestDecodeResponseNonTerminalRetryable(t *testing.T) {
	for _, status := range []string{"queued", "in_progress"} {
		_, err := New().DecodeResponse([]byte(`{"id":"r1","status":"` + status + `","output":[]}`))
		ie, ok := err.(*ir.Error)
		if !ok {
			t.Fatalf("%s：错误类型 = %T，want *ir.Error", status, err)
		}
		if ie.Type != ir.ErrTypeConnection || !ie.Retryable {
			t.Errorf("%s：got type=%q retryable=%v，want 可重试的 connection_error", status, ie.Type, ie.Retryable)
		}
	}
}

// 正常 completed 不受新增开关影响。
func TestDecodeResponseCompletedUnaffected(t *testing.T) {
	resp, err := New().DecodeResponse([]byte(`{"id":"r1","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	if err != nil {
		t.Fatalf("completed 被误判：%v", err)
	}
	if len(resp.Content) == 0 {
		t.Fatal("正文丢了")
	}
}

// R104：官方 Responses API 每帧都带 event: 行；只发 data: 的流让按事件名分发的
// 客户端一个事件都认不出。自往返回归：自家编码器的帧必须能被自家解码器认全。
func TestStreamEncoderFramesCarryEventLine(t *testing.T) {
	enc := New().NewStreamEncoder()
	var raw [][]byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "gpt"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode %v: %v", ev.Type, err)
		}
		raw = append(raw, frames...)
	}
	raw = append(raw, enc.Finish()...)
	if len(raw) == 0 {
		t.Fatal("一帧都没编出来")
	}
	dec := New().NewStreamDecoder()
	for _, fr := range raw {
		s := string(fr)
		if !strings.HasPrefix(s, "event: ") {
			t.Errorf("帧缺 event: 行：%q", s)
			continue
		}
		// 自往返：按 SSE 规则拆 event/data 喂回解码器，一个事件都不得报错。
		event := strings.TrimPrefix(strings.SplitN(s, "\n", 2)[0], "event: ")
		data := strings.TrimPrefix(strings.SplitN(strings.TrimSuffix(s, "\n\n"), "\n", 2)[1], "data: ")
		if _, err := dec.Feed(event, data); err != nil {
			t.Errorf("自家帧自家解码器认不出（event=%q）：%v", event, err)
		}
	}
}

// RenderStreamError 同样要带 event: error 行。
func TestRenderStreamErrorCarriesEventLine(t *testing.T) {
	fr := string(New().RenderStreamError(&ir.Error{Type: ir.ErrTypeRateLimit, Message: "rl"}))
	if !strings.HasPrefix(fr, "event: error\ndata: ") {
		t.Fatalf("错误帧缺 event: 行：%q", fr)
	}
}

// 错误帧已是终止帧（EvError 置 completed）：之后的 message_delta 不得再发
// response.completed，Finish 也不许补。
func TestStreamEncoderSuppressesCompletedAfterError(t *testing.T) {
	enc := New().NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gpt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeOverloaded, Message: "x"}}); err != nil {
		t.Fatal(err)
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Errorf("错误之后又发出 message_delta 帧：%q", frames)
	}
	for _, fr := range enc.Finish() {
		if strings.Contains(string(fr), "response.completed") {
			t.Errorf("错误之后 Finish 补了 completed 帧：%q", fr)
		}
	}
}

// R104 流式 created 保真：response.created 的 created_at 随 EvMessageStart 交付，
// 编码器原值回写，不得被代理本地钟覆盖。
func TestStreamCreatedAtRoundTrip(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs, err := dec.Feed("response.created",
		`{"type":"response.created","response":{"id":"r1","model":"gpt","created_at":1700000000}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Created != 1700000000 {
		t.Fatalf("EvMessageStart.Created = %+v，want 1700000000", evs)
	}
	enc := New().NewStreamEncoder()
	frames, err := enc.Encode(evs[0])
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, fr := range frames {
		joined += string(fr)
	}
	if !strings.Contains(joined, `"created_at":1700000000`) {
		t.Errorf("出站 created_at 被本地钟覆盖：%s", joined)
	}
}

// R104：未识别种类的跨族托管工具（如 anthropic 的 computer_20250124）没有同族
// 原生名可用时，canonical 原名写进 type 是必 400 的形状，整块丢弃比拒整轮诚实。
func TestEncodeRequestDropsUnmappedCrossFamilyHosted(t *testing.T) {
	out, err := New().EncodeRequest(&ir.Request{
		Model: "gpt", Stream: false,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{
			{Hosted: "computer_20250124", HostedType: "computer_20250124", Name: "computer"},
			{Hosted: "web_search_preview", HostedType: "web_search_preview", Name: "web_search_preview"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Tools) != 1 || wire.Tools[0].Type != "web_search_preview" {
		t.Fatalf("跨族托管工具没被丢弃/同族没保留：%s", out)
	}
}

// R104：function_call_output 的 output 是 Required 键——纯图片/空文本的工具
// 结果也必须写 ""，否则是 {"type":"function_call_output","call_id":...} 非法形状。
func TestEncodeRequestToolResultAlwaysWritesOutputKey(t *testing.T) {
	out, err := New().EncodeRequest(&ir.Request{
		Model: "gpt",
		// 工具必须声明：未声明的工具调用会被规整流水线降级成文本，
		// 走不到 function_call_output 分支。
		Tools: []ir.Tool{{Name: "snap", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: "call_1", Name: "snap", Input: json.RawMessage(`{}`),
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: "call_1",
				Content:   []ir.Block{{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "aGk="}}},
			}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Input []struct {
			Type   string          `json:"type"`
			Output json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	for _, it := range wire.Input {
		if it.Type == "function_call_output" {
			if string(it.Output) != `""` {
				t.Fatalf("纯图片工具结果的 output 键 = %s，want \"\"", it.Output)
			}
			return
		}
	}
	t.Fatalf("function_call_output 项没编出来：%s", out)
}

// R104：prompt_cache_retention 同族往返（OpenAI 两系同形同值集）。
func TestPromptCacheRetentionRoundTrip(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"model":"gpt","prompt_cache_retention":"24h",` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.PromptCacheRetention != "24h" {
		t.Fatalf("入站 retention 没落 IR：%q", req.PromptCacheRetention)
	}
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"prompt_cache_retention":"24h"`) {
		t.Errorf("出站 retention 丢了：%s", out)
	}
}
