package anthropic

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

const r110Iter = `{"message":{"input_tokens":10},"compaction":{"input_tokens":3}}`

// R110-C6 anthropic beta usage.iterations（按 message/compaction/advisor 迭代
// 阶段细分的用量）原文透传：同族非流式往返逐字带回。
func TestR110IterationsNonStreamRoundTrip(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":2,"iterations":` + r110Iter + `}}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Usage.Iterations) == 0 {
		t.Fatal("usage.iterations 没落进 IR")
	}
	if !strings.Contains(string(resp.Usage.Iterations), "compaction") {
		t.Fatalf("iterations 原文被改写：%s", resp.Usage.Iterations)
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"iterations"`) || !strings.Contains(string(out), "compaction") {
		t.Errorf("同族往返丢了 usage.iterations：%s", out)
	}
}

// 流式解码：message_start 与 message_delta 的 usage.iterations 都要保住
// （convUsage / convDeltaUsage 两条路径）。
func TestR110IterationsStreamDecode(t *testing.T) {
	d := &streamDecoder{}
	startEvs, err := d.Feed("message_start",
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"iterations":`+r110Iter+`}}}`)
	if err != nil {
		t.Fatalf("Feed(message_start): %v", err)
	}
	var startUsage *ir.Usage
	for _, ev := range startEvs {
		if ev.Type == ir.EvMessageStart {
			startUsage = ev.Usage
		}
	}
	if startUsage == nil || !strings.Contains(string(startUsage.Iterations), "compaction") {
		t.Fatalf("message_start usage.iterations 丢了：%+v", startUsage)
	}

	deltaEvs, err := d.Feed("message_delta",
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"iterations":`+r110Iter+`}}`)
	if err != nil {
		t.Fatalf("Feed(message_delta): %v", err)
	}
	var deltaUsage *ir.Usage
	for _, ev := range deltaEvs {
		if ev.Type == ir.EvMessageDelta {
			deltaUsage = ev.Usage
		}
	}
	if deltaUsage == nil || !strings.Contains(string(deltaUsage.Iterations), "compaction") {
		t.Fatalf("message_delta usage.iterations 丢了：%+v", deltaUsage)
	}
}

// 流式编码：message_start 与 message_delta 帧都要写回 iterations
// （messageStartUsage / encodeDeltaUsagePtr 两条路径）。
func TestR110IterationsStreamEncode(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	iter := json.RawMessage(r110Iter)
	var sb strings.Builder
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m",
			Usage: &ir.Usage{InputTokens: 1, Iterations: iter}},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn,
			Usage: &ir.Usage{OutputTokens: 2, Iterations: iter}},
		{Type: ir.EvMessageStop},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		for _, f := range frames {
			sb.Write(f)
		}
	}
	s := sb.String()
	if n := strings.Count(s, `"iterations"`); n < 2 {
		t.Errorf("message_start 与 message_delta 都应写回 iterations，只出现 %d 次：\n%s", n, s)
	}
}

// 没给 iterations 时不凭空写出键（omitempty）：stable 往返不该被 beta 字段污染。
func TestR110IterationsAbsentStaysQuiet(t *testing.T) {
	resp, err := New().DecodeResponse([]byte(`{"id":"msg_1","type":"message","role":"assistant",` +
		`"model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":2}}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Usage.Iterations) != 0 {
		t.Fatalf("缺席却解出了 iterations：%s", resp.Usage.Iterations)
	}
	out, _ := New().EncodeResponse(resp)
	if strings.Contains(string(out), "iterations") {
		t.Errorf("缺席却写出 iterations 键：%s", out)
	}
}

// R110-A3 响应侧图片块：官方响应 ContentBlock 联合（stable+beta）没有 image
// 成员，assistant 回合不产出附件。跨族上游回的图片整块跳过 + 计数报出。
// （document 形态由 document_test.go 覆盖，这里补 image 形态的流式路径。）
func TestR110StreamImageDroppedOnResponseSide(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	var sb strings.Builder
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0,
			Block: &ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "SU1H"}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 1}},
		{Type: ir.EvMessageStop},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		for _, f := range frames {
			sb.Write(f)
		}
	}
	s := sb.String()
	// 图片本体不上线（base64 与 image 判别式都不该出现）。
	for _, gone := range []string{"SU1H", `"type":"image"`} {
		if strings.Contains(s, gone) {
			t.Errorf("响应侧仍下发了应丢弃的图片 %s：%s", gone, s)
		}
	}
	notes := enc.Notes()
	if want := proto.MediaOutputDropNote(1, 0); !slices.Contains(notes, want) {
		t.Errorf("未报图片块丢弃：notes=%q，want 含 %q", notes, want)
	}
	// 排干后不重复报。
	if again := enc.Notes(); len(again) != 0 {
		t.Errorf("Notes() 未排干：%q", again)
	}
}
