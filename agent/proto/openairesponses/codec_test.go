package openairesponses

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// 请求方向：仅本族形态签名可还原 reasoning item（encrypted_content）；
// 外族/来源不明的签名不落线体，思考正文降级成 output_text 文本保住。
func TestEncodeRequest_ReasoningItem(t *testing.T) {
	mk := func(sig, from string) *ir.Request {
		return &ir.Request{
			Model: "gpt-x",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
				{Role: ir.RoleAssistant, Content: []ir.Block{
					{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm", Signature: sig, SignatureFrom: from}},
					{Type: ir.BlockText, Text: "ok"},
				}},
				{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
			},
		}
	}

	out, err := New().EncodeRequest(mk("enc", "openai-responses"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"encrypted_content"`) {
		t.Errorf("same-protocol signature should round-trip as reasoning item: %s", out)
	}

	for _, from := range []string{"anthropic", ""} {
		out, err := New().EncodeRequest(mk("enc", from))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), `"encrypted_content"`) {
			t.Errorf("foreign signature (from=%q) must not become a reasoning item: %s", from, out)
		}
		if !strings.Contains(string(out), "hmm") {
			t.Errorf("外族签名的思考正文应降级成文本保住 (from=%q)：%s", from, out)
		}
	}
}

// DecodeRequest 官方 Responses input 形态全兼容：
// 纯字符串、省略 type 的 message item（role 推断）、带 type 的标准形态。
func TestDecodeRequest_InputShapes(t *testing.T) {
	mustUser := func(t *testing.T, body string) *ir.Request {
		t.Helper()
		req, err := New().DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser {
			t.Fatalf("messages = %+v, want single user message", req.Messages)
		}
		if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Text != "hi" {
			t.Fatalf("content = %+v, want text hi", req.Messages[0].Content)
		}
		return req
	}

	// 纯字符串 input（最简形态）
	mustUser(t, `{"model":"m","input":"hi"}`)
	// 字符串 input + instructions 进 system
	req := mustUser(t, `{"model":"m","instructions":"be nice","input":"hi"}`)
	if len(req.System) != 1 {
		t.Fatalf("system = %+v, want instructions", req.System)
	}
	// 数组 item 省略 type（role 推断为 message）
	mustUser(t, `{"model":"m","input":[{"role":"user","content":"hi"}]}`)
	// 数组 item 带 type + content 纯字符串
	mustUser(t, `{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`)
	// 数组 item 带 type + content parts
	mustUser(t, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	// 无 role 无 type 的未知 item 不产生消息（也不报错）
	req, err := New().DecodeRequest([]byte(`{"model":"m","input":[{"id":"it_1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 0 {
		t.Fatalf("unknown items must be skipped, got %+v", req.Messages)
	}
}

// compaction_trigger 条目：置 Compact 且不进消息流；普通请求不置位。
func TestDecodeRequest_CompactionTrigger(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"model":"m","input":[{"role":"user","content":"hi"},{"type":"compaction_trigger"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !req.Compact {
		t.Fatal("compaction_trigger item must set req.Compact")
	}
	if len(req.Messages) != 1 {
		t.Fatalf("compaction_trigger must not enter message stream, messages = %+v", req.Messages)
	}

	req, err = New().DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Compact {
		t.Fatal("plain request must not set req.Compact")
	}
}

// codex 别名 codec：协议名独立注册，编解码与 openai-responses 同一实现；
// instructions 恒存在（订阅端点要求字段，无 system 时输出空串）。
func TestCodexCodec(t *testing.T) {
	c := New()
	cx := codexCodec{codec: codec{subscriptionCompat: true}}
	if c.Name() != "openai-responses" || cx.Name() != NameCodex {
		t.Fatalf("codec names = %q / %q", c.Name(), cx.Name())
	}
	if _, err := proto.GetOutbound(NameCodex); err != nil {
		t.Fatalf("codex codec not registered: %v", err)
	}
	out, err := cx.EncodeRequest(&ir.Request{
		Model:    "gpt-5.6-sol",
		Stream:   true,
		Thinking: &ir.ThinkingConfig{Enabled: true, Effort: "xhigh"},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"instructions":`, `"store":false`, `"effort":"xhigh"`, `reasoning.encrypted_content`} {
		if !strings.Contains(s, want) {
			t.Fatalf("encoded missing %s: %s", want, s)
		}
	}
}

func TestStreamDecodeFunctionArgumentsDoneWithoutDeltas(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("", `{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"id\":1}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvToolInput || evs[0].Text != `{"id":1}` {
		t.Fatalf("done-only events = %+v", evs)
	}
	evs, err = dec.Feed("", `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvBlockStop {
		t.Fatalf("output_item.done duplicated arguments: %+v", evs)
	}
}

func TestStreamDecodeFunctionArgumentsDoneCompletesDeltaPrefix(t *testing.T) {
	dec := New().NewStreamDecoder()
	_, _ = dec.Feed("", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`)
	evs, err := dec.Feed("", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"id\":"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Text != `{"id":` {
		t.Fatalf("delta events = %+v", evs)
	}
	evs, err = dec.Feed("", `{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"id\":1}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Text != `1}` {
		t.Fatalf("done suffix events = %+v", evs)
	}
	evs, err = dec.Feed("", `{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"id\":1}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("repeated done duplicated arguments: %+v", evs)
	}
}

func TestStreamDecodeOutputItemDoneBackfillsArguments(t *testing.T) {
	dec := New().NewStreamDecoder()
	_, _ = dec.Feed("", `{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_2","name":"lookup"}}`)
	evs, err := dec.Feed("", `{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{\"id\":2}"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Type != ir.EvToolInput || evs[0].Text != `{"id":2}` || evs[1].Type != ir.EvBlockStop {
		t.Fatalf("output_item.done events = %+v", evs)
	}
}

func TestStreamDecodeOutputItemAddedCarriesArguments(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs, err := dec.Feed("", `{"type":"response.output_item.added","output_index":3,"item":{"type":"function_call","call_id":"call_3","name":"lookup","arguments":"{\"id\":3}"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Type != ir.EvBlockStart || evs[1].Type != ir.EvToolInput || evs[1].Text != `{"id":3}` {
		t.Fatalf("output_item.added events = %+v", evs)
	}
}

func TestStreamDecodeFunctionArgumentsDoneRejectsNonSuffix(t *testing.T) {
	for _, full := range []string{`{"id":`, `{"name":1}`} {
		dec := New().NewStreamDecoder()
		_, _ = dec.Feed("", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"id\":1}"}`)
		evs, err := dec.Feed("", `{"type":"response.function_call_arguments.done","output_index":0,"arguments":`+strconv.Quote(full)+`}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != 0 {
			t.Fatalf("non-suffix full %q appended events: %+v", full, evs)
		}
	}
}

func TestCustomToolRequestRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"custom","name":"shell","description":"run command","format":{"type":"grammar","syntax":"lark","definition":"start: WORD"}}],"tool_choice":{"type":"custom","name":"shell"},"input":[{"type":"custom_tool_call","id":"ctc_item","call_id":"call_1","name":"shell","input":"echo hi"},{"type":"custom_tool_call_output","call_id":"call_1","output":"hi"}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Kind != ir.ToolCustom || req.Tools[0].Name != "shell" || !json.Valid(req.Tools[0].Format) {
		t.Fatalf("custom tool = %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ChoiceTool || req.ToolChoice.ToolKind != ir.ToolCustom || req.ToolChoice.ToolName != "shell" {
		t.Fatalf("tool choice = %+v", req.ToolChoice)
	}
	var call *ir.ToolUse
	var result *ir.ToolResult
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.ToolUse != nil {
				call = b.ToolUse
			}
			if b.ToolResult != nil {
				result = b.ToolResult
			}
		}
	}
	if call == nil || call.Kind != ir.ToolCustom || call.InputText != "echo hi" || string(call.ObjectInput()) != `{"input":"echo hi"}` {
		t.Fatalf("custom call = %+v", call)
	}
	if result == nil || result.Kind != ir.ToolCustom || result.ToolUseID != "call_1" {
		t.Fatalf("custom result = %+v", result)
	}
	encoded, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out := string(encoded)
	for _, want := range []string{`"type":"custom"`, `"format":{"type":"grammar"`, `"type":"custom_tool_call"`, `"input":"echo hi"`, `"type":"custom_tool_call_output"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("round-trip missing %s: %s", want, out)
		}
	}
}

func TestCustomToolResponseRoundTrip(t *testing.T) {
	resp, err := New().DecodeResponse([]byte(`{"id":"resp_1","model":"m","status":"completed","output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"shell","input":"echo hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
		t.Fatalf("content = %+v", resp.Content)
	}
	call := resp.Content[0].ToolUse
	if call.Kind != ir.ToolCustom || call.InputText != "echo hi" || call.ID != "call_1" {
		t.Fatalf("custom response call = %+v", call)
	}
	encoded, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	out := string(encoded)
	if !strings.Contains(out, `"type":"custom_tool_call"`) || !strings.Contains(out, `"input":"echo hi"`) {
		t.Fatalf("encoded response lost custom call: %s", out)
	}
}

func TestStreamDecodeCustomToolDoneOnlyAndPrefix(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs, err := dec.Feed("", `{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"shell"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvBlockStart || evs[0].Block.ToolUse.Kind != ir.ToolCustom {
		t.Fatalf("custom start = %+v", evs)
	}
	evs, err = dec.Feed("", `{"type":"response.custom_tool_call_input.done","output_index":0,"input":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvToolInput || evs[0].Text != "echo hi" {
		t.Fatalf("done-only input = %+v", evs)
	}
	evs, err = dec.Feed("", `{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"shell","input":"echo hi"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvBlockStop {
		t.Fatalf("custom done duplicated input: %+v", evs)
	}

	dec = New().NewStreamDecoder()
	_, _ = dec.Feed("", `{"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","call_id":"call_2","name":"shell"}}`)
	evs, _ = dec.Feed("", `{"type":"response.custom_tool_call_input.delta","output_index":1,"delta":"echo "}`)
	if len(evs) != 1 || evs[0].Text != "echo " {
		t.Fatalf("custom delta = %+v", evs)
	}
	evs, _ = dec.Feed("", `{"type":"response.custom_tool_call_input.done","output_index":1,"input":"echo hi"}`)
	if len(evs) != 1 || evs[0].Text != "hi" {
		t.Fatalf("custom done suffix = %+v", evs)
	}
}

func TestStreamEncodeCustomToolNativeEvents(t *testing.T) {
	enc := New().NewStreamEncoder()
	var frames [][]byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "call_1", Name: "shell", Kind: ir.ToolCustom}}},
		{Type: ir.EvToolInput, Index: 0, Text: "echo "},
		{Type: ir.EvToolInput, Index: 0, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 0},
	} {
		out, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, out...)
	}
	joined := string(bytesJoin(frames))
	for _, want := range []string{"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", `"type":"custom_tool_call"`, `"input":"echo hi"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stream missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "function_call_arguments") {
		t.Fatalf("custom stream emitted function events: %s", joined)
	}
}

func bytesJoin(parts [][]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
