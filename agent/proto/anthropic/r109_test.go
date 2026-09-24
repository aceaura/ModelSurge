package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R109-A4 配套 max_messages 外族归档：responses 的消息数上限档 Anthropic
// 没有对应值，同 aborted 取 max_tokens（输出确实不完整）。
func TestR109MaxMessagesUnmap(t *testing.T) {
	if got := UnmapStopReason(ir.StopMaxMessages); got != "max_tokens" {
		t.Fatalf("UnmapStopReason(StopMaxMessages) = %q", got)
	}
}

// R109-A5 流式 compaction_delta 独立计数：官方类型、语义清楚但 IR 无槽位，
// 与「连语义都不认识的型」分账，两种帧同流时两条注记各报各的。
func TestR109CompactionDeltaNote(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"compaction_delta","encrypted_content":"enc_abc"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"totally_new_delta","x":1}}`); err != nil {
		t.Fatal(err)
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "dropped 1 compaction delta(s)") {
		t.Fatalf("compaction 注记缺失：%q", joined)
	}
	if !strings.Contains(joined, "ignored 1 stream event(s)") {
		t.Fatalf("未知型注记缺失：%q", joined)
	}
	// 排干后不再重复报。
	if again := dec.(interface{ Notes() []string }).Notes(); len(again) != 0 {
		t.Fatalf("Notes() 未排干：%q", again)
	}
	// 没给 compaction_delta 时闭嘴。
	quiet := New().NewStreamDecoder()
	if _, err := quiet.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`); err != nil {
		t.Fatal(err)
	}
	if n := quiet.(interface{ Notes() []string }).Notes(); len(n) != 0 {
		t.Fatalf("正常流误报：%q", n)
	}
}

// R109-A6 message_delta 顶层 context_management 回执：流式解码进事件、
// 同族编码原样回写；没给不造键。
func TestR109ContextMgmtDeltaRoundTrip(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("message_start", `{"type":"message_start","message":{"id":"m","model":"c"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("message_delta",
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},`+
			`"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || string(evs[0].ContextMgmt) == "" {
		t.Fatalf("ContextMgmt 没进事件：%+v", evs)
	}

	enc := New().NewStreamEncoder()
	frames, err := enc.Encode(evs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || !strings.Contains(string(frames[0]), `"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3}]}`) {
		t.Fatalf("回执没原样回写：%q", frames)
	}

	// 没带回执的 message_delta 不造键。
	frames2, err := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(frames2[0]), "context_management") {
		t.Fatalf("没给被伪造：%q", frames2[0])
	}
}

// R109-B2 请求级 output_format（beta 旧槽位）：与 output_config.format 同判据
// 收下；两槽同给新槽胜出；编码恒写新槽不产出旧键。
func TestR109OutputFormatLegacySlot(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],` +
		`"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.ResponseFormat == nil || !strings.Contains(string(req.ResponseFormat.Schema), `"properties"`) {
		t.Fatalf("output_format 没进 IR：%+v", req.ResponseFormat)
	}
	// 编码写新槽 output_config.format，不产出废弃旧键。
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"output_config"`) || strings.Contains(string(out), `"output_format"`) {
		t.Fatalf("应写新槽不写旧键：%s", out)
	}

	// 两槽同给：output_config.format 胜出。
	both, err := New().DecodeRequest([]byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}],` +
		`"output_format":{"type":"json_schema","schema":{"type":"object","properties":{"old":{"type":"string"}}}},` +
		`"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"new":{"type":"string"}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if both.ResponseFormat == nil || !strings.Contains(string(both.ResponseFormat.Schema), `"new"`) {
		t.Fatalf("新槽未胜出：%+v", both.ResponseFormat)
	}
}

// R109-B1 外来投影引用缺 encrypted_index 整条丢弃：Required 字段缺键上游
// 必 400，宁丢不伪造；带 encrypted_index 的投影与带 Raw 的同族引用不受影响。
func TestR109CitationEncryptedIndexRequired(t *testing.T) {
	text := "hello world"
	// 投影引用：有 URL、cited_text 可反推，但没有 encrypted_index——此前会
	// 带空 encrypted_index 发出去整轮 400。
	out, dropped := encodeCitations(text, []ir.Citation{{URL: "https://x", Start: 0, End: 5}})
	if dropped != 1 || len(out) != 0 {
		t.Fatalf("缺 encrypted_index 应整条丢弃：out=%d dropped=%d", len(out), dropped)
	}
	// 带 encrypted_index 的投影正常落 web_search_result_location。
	out2, dropped2 := encodeCitations(text, []ir.Citation{{URL: "https://x", Start: 0, End: 5, EncryptedIndex: "enc_1"}})
	if dropped2 != 0 || len(out2) != 1 || !strings.Contains(string(out2[0]), `"encrypted_index":"enc_1"`) {
		t.Fatalf("带 encrypted_index 应保留：out=%v dropped=%d", out2, dropped2)
	}
	// 带 Raw 的同族引用原文带回，不受投影判据影响。
	raw := json.RawMessage(`{"type":"char_location","cited_text":"w","document_index":0,"start_char_index":1,"end_char_index":2,"encrypted_index":"e"}`)
	out3, dropped3 := encodeCitations(text, []ir.Citation{{Raw: raw}})
	if dropped3 != 0 || len(out3) != 1 || string(out3[0]) != string(raw) {
		t.Fatalf("Raw 引用应原文带回：out=%v dropped=%d", out3, dropped3)
	}
}
