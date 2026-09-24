package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R109-A3 思考块开块补发 reasoning_summary_part.added：Codex 严格客户端只在
// part.added 之后渲染 summary delta，缺这帧整条思考摘要在客户端不可见。
func TestR109SummaryPartAddedFrame(t *testing.T) {
	enc := New().NewStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0,
		Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("开块应发 output_item.added + summary_part.added 两帧，实发 %d", len(frames))
	}
	second := frameData(t, frames[1])
	if second["type"] != "response.reasoning_summary_part.added" {
		t.Fatalf("第二帧类型 = %v", second["type"])
	}
	if si, ok := second["summary_index"].(float64); !ok || si != 0 {
		t.Fatalf("summary_index 应为 0：%v", second["summary_index"])
	}
	part, _ := second["part"].(map[string]any)
	if part["type"] != "summary_text" {
		t.Fatalf("part.type = %v", part["type"])
	}
}

// R109-A4 incomplete_details.reason=max_messages 独立档位：与 max_tokens 两回事，
// 不并档；编码回写原值。
func TestR109MaxMessagesIncomplete(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("response.created", `{"type":"response.created","response":{"id":"r1","model":"gpt"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("response.incomplete",
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_messages"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got ir.StopReason
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopMaxMessages {
		t.Fatalf("max_messages 应成独立档：%q", got)
	}
	// 回写原值（不塌成 max_output_tokens）。
	if unmapIncompleteReason(ir.StopMaxMessages) != "max_messages" {
		t.Fatal("unmapIncompleteReason 未回写 max_messages")
	}
}

// R109-A7 多段 reasoning summary 合并计数：summary_index>0 的帧正文进 IR 但
// 边界变形必须报得出；index 0 的正常帧闭嘴。
func TestR109MergedSummaryNote(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("response.output_item.added",
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("response.reasoning_summary_part.added",
		`{"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"rs_1","summary_index":0,"part":{"type":"summary_text","text":""}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("response.reasoning_summary_text.delta",
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","summary_index":0,"delta":"第一段"}`); err != nil {
		t.Fatal(err)
	}
	// summary_index>0 的 delta 与 done 各计一次。
	if _, err := dec.Feed("response.reasoning_summary_text.delta",
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","summary_index":1,"delta":"第二段"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("response.reasoning_summary_part.done",
		`{"type":"response.reasoning_summary_part.done","output_index":0,"item_id":"rs_1","summary_index":1,"part":{"type":"summary_text","text":"第二段"}}`); err != nil {
		t.Fatal(err)
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "merged 2 reasoning summary frame(s)") {
		t.Fatalf("合并注记缺失：%q", joined)
	}

	// 只有 index 0 的流闭嘴。
	quiet := New().NewStreamDecoder()
	if _, err := quiet.Feed("response.output_item.added",
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := quiet.Feed("response.reasoning_summary_text.delta",
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_1","summary_index":0,"delta":"只有一段"}`); err != nil {
		t.Fatal(err)
	}
	if n := quiet.(interface{ Notes() []string }).Notes(); len(n) != 0 {
		t.Fatalf("单段 summary 误报：%q", n)
	}
}
