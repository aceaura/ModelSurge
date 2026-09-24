package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R107-甲2 function_call/custom_tool_call/reasoning/message 的 item id 同族
// 往返：store=true 时上游存的条目按 item id 索引，换成合成 id 后
// item_reference 全部错指。
func TestItemIDRoundTrip(t *testing.T) {
	body := `{"model":"gpt-x","tools":[` +
		`{"type":"function","name":"get","parameters":{"type":"object"}},` +
		`{"type":"custom","name":"free","description":"freeform"}],"input":[` +
		`{"type":"message","id":"msg_aaa","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"message","id":"msg_bbb","role":"assistant","content":[{"type":"output_text","text":"ok"}]},` +
		`{"type":"reasoning","id":"rs_ccc","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"enc"},` +
		`{"type":"function_call","id":"fc_ddd","call_id":"call_1","name":"get","arguments":"{}"},` +
		`{"type":"custom_tool_call","id":"ctc_eee","call_id":"call_2","name":"free","input":"raw text"}` +
		`]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var msgUser, msgAsst, rsID, fcID, ctcID string
	for _, m := range req.Messages {
		if m.ItemID != "" {
			if m.Role == ir.RoleUser {
				msgUser = m.ItemID
			} else {
				msgAsst = m.ItemID
			}
		}
		for _, b := range m.Content {
			if b.Type == ir.BlockThinking && b.Thinking != nil {
				rsID = b.Thinking.ItemID
			}
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				if b.ToolUse.Kind == ir.ToolCustom {
					ctcID = b.ToolUse.ItemID
				} else {
					fcID = b.ToolUse.ItemID
				}
			}
		}
	}
	if msgUser != "msg_aaa" || msgAsst != "msg_bbb" || rsID != "rs_ccc" || fcID != "fc_ddd" || ctcID != "ctc_eee" {
		t.Fatalf("item ids dropped at decode: msg=%q/%q rs=%q fc=%q ctc=%q", msgUser, msgAsst, rsID, fcID, ctcID)
	}

	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"id":"msg_aaa"`, `"id":"msg_bbb"`, `"id":"rs_ccc"`, `"id":"fc_ddd"`, `"id":"ctc_eee"`} {
		if !strings.Contains(s, want) {
			t.Errorf("re-encoded request missing %s: %s", want, s)
		}
	}

	// 对照组：没给 id 的 item 不得发明 id 键（非流式编码不合成）。
	plain, err := New().DecodeRequest([]byte(
		`{"model":"gpt-x","input":[{"type":"function_call","call_id":"call_1","name":"get","arguments":"{}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	pout, err := New().EncodeRequest(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pout), `"id":"fc_`) {
		t.Errorf("absent item id must not be invented: %s", pout)
	}
}

// R107-甲2 流式解码的 item id 落 IR，流式编码优先使用而非合成。
func TestStreamItemIDRoundTrip(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-x"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_real","call_id":"call_1","name":"get","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_real","call_id":"call_1","name":"get","arguments":"{}"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_real","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_real","summary":[],"encrypted_content":"enc"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
	)
	var fcID, rsID string
	for _, ev := range evs {
		if ev.Type != ir.EvBlockStart || ev.Block == nil {
			continue
		}
		if ev.Block.Type == ir.BlockToolUse && ev.Block.ToolUse != nil {
			fcID = ev.Block.ToolUse.ItemID
		}
		if ev.Block.Type == ir.BlockThinking && ev.Block.Thinking != nil {
			rsID = ev.Block.Thinking.ItemID
		}
	}
	if fcID != "fc_real" || rsID != "rs_real" {
		t.Fatalf("stream decode dropped item ids: fc=%q rs=%q", fcID, rsID)
	}

	// 编码侧：带着 ItemID 开块，added 帧必须用原号而不是 fc_0001 合成号。
	enc := codec{}.NewStreamEncoder()
	var out []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt-x"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "call_1", Name: "get", ItemID: "fc_real"}}},
		{Type: ir.EvToolInput, Index: 0, Text: "{}"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockThinking,
			Thinking: &ir.Thinking{Text: "t", Signature: "enc", SignatureFrom: Name, ItemID: "rs_real"}}},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			out = append(out, f...)
		}
	}
	for _, f := range enc.Finish() {
		out = append(out, f...)
	}
	s := string(out)
	if !strings.Contains(s, `"id":"fc_real"`) || !strings.Contains(s, `"id":"rs_real"`) {
		t.Errorf("stream encode did not carry original item ids:\n%s", s)
	}
	if strings.Contains(s, `"id":"fc_0001"`) || strings.Contains(s, `"id":"rs_0001"`) {
		t.Errorf("stream encode synthesized ids despite ItemID:\n%s", s)
	}
}

// R107-甲3 web_search_call.status 双向：failed/incomplete 不再伪造成
// completed；外族错误码归 failed；成功路径恒 completed 不变。
func TestWebSearchStatusRoundTrip(t *testing.T) {
	// 解码：status=failed 落 ErrorCode，不解成「成功但没找到」
	body := `{"model":"gpt-x","input":[{"type":"web_search_call","id":"ws_1","status":"failed",` +
		`"action":{"type":"search","query":"x","sources":[]}}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var errCode string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockWebSearchToolResult && b.WebSearchToolResult != nil {
				errCode = b.WebSearchToolResult.ErrorCode
			}
		}
	}
	if errCode != "failed" {
		t.Fatalf("failed status decoded as ErrorCode = %q", errCode)
	}

	// 非流式编码：ErrorCode=failed -> status:"failed"；外族码 -> failed；
	// 空 -> completed
	mk := func(code string) string {
		r := &ir.Request{Model: "gpt-x", Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockServerToolUse,
				ServerToolUse: &ir.ServerToolUse{ID: "ws_1", Name: "web_search", Input: []byte(`{"query":"x"}`)}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockWebSearchToolResult,
				WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "ws_1", ErrorCode: code}}}},
		}}
		out, err := New().EncodeRequest(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if s := mk("failed"); !strings.Contains(s, `"status":"failed"`) {
		t.Errorf("failed not preserved: %s", s)
	}
	if s := mk("incomplete"); !strings.Contains(s, `"status":"incomplete"`) {
		t.Errorf("incomplete not preserved: %s", s)
	}
	if s := mk("max_uses_exceeded"); !strings.Contains(s, `"status":"failed"`) {
		t.Errorf("foreign error code must map to failed: %s", s)
	}
	if s := mk(""); !strings.Contains(s, `"status":"completed"`) {
		t.Errorf("success path changed: %s", s)
	}
}

// R107-甲3 流式编码的 done 帧 status 据配对结果块的 ErrorCode 回写。
func TestStreamWebSearchDoneStatus(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	var out []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt-x"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{ID: "ws_1", Name: "web_search", Input: []byte(`{"query":"x"}`)}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockWebSearchToolResult,
			WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "ws_1", ErrorCode: "failed"}}},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			out = append(out, f...)
		}
	}
	for _, f := range enc.Finish() {
		out = append(out, f...)
	}
	s := string(out)
	if !strings.Contains(s, `"status":"failed"`) {
		t.Errorf("done frame faked failed search as completed:\n%s", s)
	}
}

// R107-甲6 reasoning item 的 content 数组（reasoning_text）：summary 为空时
// 正文不再整条丢。非流式与流式 done 帧同判据。
func TestReasoningContentFallback(t *testing.T) {
	body := `{"model":"gpt-x","input":[{"type":"reasoning","id":"rs_1","summary":[],` +
		`"content":[{"type":"reasoning_text","text":"deep thought"}]}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockThinking && b.Thinking != nil {
				text = b.Thinking.Text
			}
		}
	}
	if text != "deep thought" {
		t.Fatalf("reasoning content dropped: %q", text)
	}

	// 流式 done 帧同款：summary 空、content 有正文。
	dec := New().NewStreamDecoder()
	var thinking []string
	for _, ev := range feedAll(t, dec,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-x"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"stream thought"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
	) {
		if ev.Type == ir.EvThinkingDelta {
			thinking = append(thinking, ev.Text)
		}
	}
	if strings.Join(thinking, "") != "stream thought" {
		t.Errorf("stream reasoning content dropped: %q", thinking)
	}
}

// R107-甲7 response.reasoning_summary_part.added 是官方事件：计进度帧账，
// 不得混进「解码器不认识」的未知账。
func TestReasoningSummaryPartAddedCountedAsProgress(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
	)
	if len(evs) != 0 {
		t.Errorf("边界标记帧不该产出 IR 事件：%+v", evs)
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "progress frame(s)") {
		t.Fatalf("官方事件被计错账：%q", notes)
	}
	if strings.Contains(notes[0], "does not know") {
		t.Errorf("官方事件被报成未知型：%q", notes[0])
	}
}
