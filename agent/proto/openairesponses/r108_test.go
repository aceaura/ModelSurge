package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// frameData 从 SSE 帧里取出 data: 行的 JSON。
func frameData(t *testing.T, frame []byte) map[string]any {
	t.Helper()
	s := string(frame)
	i := strings.Index(s, "data: ")
	if i < 0 {
		t.Fatalf("not an SSE data frame: %q", s)
	}
	line := s[i+6:]
	if j := strings.Index(line, "\n"); j >= 0 {
		line = line[:j]
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("bad frame json: %v", err)
	}
	return m
}

// R108-乙1 cache_write_tokens ↔ CacheCreationTokens 双向：官方
// input_tokens_details.cache_write_tokens 是写缓存用量，此前入站即蒸发。
func TestR108CacheWriteTokensRoundTrip(t *testing.T) {
	resp := []byte(`{"id":"r1","object":"response","created_at":1,"model":"m","status":"completed",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"usage":{"input_tokens":100,"output_tokens":5,"total_tokens":105,` +
		`"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":25}}}`)
	r, err := New().DecodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage.CacheReadTokens != 40 || r.Usage.CacheCreationTokens != 25 || r.Usage.InputTokens != 60 {
		t.Fatalf("usage = %+v", r.Usage)
	}
	out, err := New().EncodeResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"cache_write_tokens":25`) {
		t.Fatalf("cache_write_tokens 没回写：%s", out)
	}
}

// R108-乙3 metadata 回显：非流式 DecodeResponse 捕获、EncodeResponse 写回；
// 流式 response.created 帧进 EvMessageStart，编码器 created 帧带回。
func TestR108MetadataRoundTrip(t *testing.T) {
	resp := []byte(`{"id":"r1","object":"response","created_at":1,"model":"m","status":"completed",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
		`"metadata":{"trace":"xyz"},` +
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	r, err := New().DecodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Metadata) != `{"trace":"xyz"}` {
		t.Fatalf("Metadata = %s", r.Metadata)
	}
	out, err := New().EncodeResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"metadata":{"trace":"xyz"}`) {
		t.Fatalf("metadata 没回写：%s", out)
	}

	// 流式入站：created 帧的 metadata 上 EvMessageStart。
	dec := New().NewStreamDecoder()
	evs, err := dec.Feed("response.created", `{"type":"response.created","sequence_number":0,`+
		`"response":{"id":"r1","object":"response","created_at":1,"model":"m","status":"in_progress",`+
		`"metadata":{"trace":"s1"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || string(evs[0].Metadata) != `{"trace":"s1"}` {
		t.Fatalf("evs = %+v", evs)
	}

	// 流式出站：created 帧带回 metadata。
	enc := New().NewStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m",
		Metadata: json.RawMessage(`{"trace":"s1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := frameData(t, frames[0])
	respObj, _ := m["response"].(map[string]any)
	if respObj["metadata"] == nil {
		t.Fatalf("created 帧没带 metadata：%s", frames[0])
	}
}

// R108-乙7 typed tool_choice 不透明槽：{"type":"file_search"} 等无 name 键的
// 官方变体同族一个字节不动往返；此前整条 tool_choice 静默丢失。
func TestR108TypedToolChoiceRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user",` +
		`"content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"file_search","vector_store_ids":["vs_1"]}],` +
		`"tool_choice":{"type":"file_search"}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	tc := req.ToolChoice
	if tc == nil || string(tc.Raw) != `{"type":"file_search"}` {
		t.Fatalf("ToolChoice = %+v", tc)
	}
	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"tool_choice":{"type":"file_search"}`) {
		t.Fatalf("typed tool_choice 没原样回写：%s", out)
	}

	// mcp 变体（含 server_label 等额外键）同样整个留住。
	body2 := []byte(`{"model":"m","input":[],"tool_choice":{"type":"mcp","server_label":"dmcp"}}`)
	req2, err := New().DecodeRequest(body2)
	if err != nil {
		t.Fatal(err)
	}
	if req2.ToolChoice == nil || string(req2.ToolChoice.Raw) != `{"type":"mcp","server_label":"dmcp"}` {
		t.Fatalf("mcp ToolChoice = %+v", req2.ToolChoice)
	}
}

// R108-甲3 编码帧三键：sequence_number 全帧单调递增、delta/done 帧带
// item_id、annotation.added 带 per-part annotation_index。
func TestR108FrameSequenceItemAnnotation(t *testing.T) {
	enc := New().NewStreamEncoder()
	feed := func(ev ir.Event) [][]byte {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		return frames
	}
	var all [][]byte
	all = append(all, feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"})...)
	all = append(all, feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})...)
	all = append(all, feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "he"})...)
	all = append(all, feed(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{
		{URL: "https://a.example", Title: "A"},
		{URL: "https://b.example", Title: "B"},
	}})...)
	all = append(all, feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "llo"})...)
	all = append(all, feed(ir.Event{Type: ir.EvBlockStop, Index: 0})...)
	all = append(all, feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})...)

	// sequence_number 严格递增且首帧从 0 起。
	var itemID string
	var annIdx []float64
	for i, f := range all {
		m := frameData(t, f)
		sn, ok := m["sequence_number"].(float64)
		if !ok || int(sn) != i {
			t.Fatalf("frame %d sequence_number = %v: %s", i, m["sequence_number"], f)
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "response.output_text.delta", "response.output_text.done",
			"response.content_part.added", "response.content_part.done",
			"response.output_text.annotation.added":
			id, _ := m["item_id"].(string)
			if id == "" {
				t.Fatalf("%s 帧缺 item_id：%s", typ, f)
			}
			if itemID == "" {
				itemID = id
			}
			if id != itemID {
				t.Fatalf("%s 帧 item_id 漂移：%q vs %q", typ, id, itemID)
			}
			if typ == "response.output_text.annotation.added" {
				v, ok := m["annotation_index"].(float64)
				if !ok {
					t.Fatalf("annotation.added 缺 annotation_index：%s", f)
				}
				annIdx = append(annIdx, v)
			}
		}
	}
	if len(annIdx) != 2 || annIdx[0] != 0 || annIdx[1] != 1 {
		t.Fatalf("annotation_index = %v", annIdx)
	}
}

// R108-甲4 response.audio.* 归位：transcript.delta 是音频流唯一可读通道，
// 进 EvTextDelta；audio.delta 字节计数注记；两个 done 与
// compaction.compacting 归进度帧计数，不落进 unknown。
func TestR108AudioEventsRouted(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("response.created", `{"type":"response.created","sequence_number":0,`+
		`"response":{"id":"r1","object":"response","created_at":1,"model":"m","status":"in_progress"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("response.audio.transcript.delta", `{"type":"response.audio.transcript.delta","sequence_number":1,"delta":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[len(evs)-1].Type != ir.EvTextDelta || evs[len(evs)-1].Text != "hello" {
		t.Fatalf("transcript.delta evs = %+v", evs)
	}
	for _, data := range []string{
		`{"type":"response.audio.delta","sequence_number":2,"delta":"QUJD"}`,
		`{"type":"response.audio.done","sequence_number":3}`,
		`{"type":"response.audio.transcript.done","sequence_number":4}`,
		`{"type":"response.compaction.compacting","sequence_number":5}`,
	} {
		if _, err := dec.Feed("", data); err != nil {
			t.Fatal(err)
		}
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	var audioNote, progressNote bool
	for _, n := range notes {
		if strings.Contains(n, "audio frame") {
			audioNote = true
		}
		if strings.Contains(n, "progress frame") {
			progressNote = true
		}
		if strings.Contains(n, "does not know") {
			t.Fatalf("官方 audio/compaction 事件被报成未知帧：%v", notes)
		}
	}
	if !audioNote || !progressNote {
		t.Fatalf("notes = %v", notes)
	}
}

// R108-甲5 output_text.delta / done 帧携带的 logprobs 计数注记；
// 没带的流静默。
func TestR108DeltaLogprobsNoted(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("response.created", `{"type":"response.created","sequence_number":0,`+
		`"response":{"id":"r1","object":"response","created_at":1,"model":"m","status":"in_progress"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("", `{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1",`+
		`"output_index":0,"content_index":0,"delta":"hi","logprobs":[{"token":"hi","logprob":-0.1}]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("", `{"type":"response.output_text.done","sequence_number":2,"item_id":"msg_1",`+
		`"output_index":0,"content_index":0,"text":"hi","logprobs":[{"token":"hi","logprob":-0.1}]}`); err != nil {
		t.Fatal(err)
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "logprobs") {
		t.Fatalf("notes = %v", notes)
	}

	dec2 := New().NewStreamDecoder()
	if _, err := dec2.Feed("response.created", `{"type":"response.created","sequence_number":0,`+
		`"response":{"id":"r1","object":"response","created_at":1,"model":"m","status":"in_progress"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec2.Feed("", `{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1",`+
		`"output_index":0,"content_index":0,"delta":"hi"}`); err != nil {
		t.Fatal(err)
	}
	if n := dec2.(interface{ Notes() []string }).Notes(); len(n) != 0 {
		t.Fatalf("没给被报：%v", n)
	}
}
