package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// functionCall/functionResponse 的 ID 合成与按名顺序配对。
func TestDecodeRequest_FunctionCallIDPairing(t *testing.T) {
	body := `{
	  "contents": [
	    {"role": "user", "parts": [{"text": "天气和股票"}]},
	    {"role": "model", "parts": [
	      {"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}},
	      {"functionCall": {"name": "get_weather", "args": {"city": "Tokyo"}}},
	      {"functionCall": {"name": "get_stock", "args": {"symbol": "GOOG"}}}
	    ]},
	    {"role": "user", "parts": [
	      {"functionResponse": {"name": "get_weather", "response": {"result": "sunny"}}},
	      {"functionResponse": {"name": "get_weather", "response": {"result": "rainy"}}},
	      {"functionResponse": {"name": "get_stock", "response": {"price": 100}}}
	    ]}
	  ]
	}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	model := req.Messages[1]
	if model.Role != ir.RoleAssistant {
		t.Fatalf("model role = %q", model.Role)
	}
	var callIDs []string
	for _, b := range model.Content {
		if b.Type != ir.BlockToolUse {
			t.Fatalf("unexpected block %v", b.Type)
		}
		callIDs = append(callIDs, b.ToolUse.ID)
	}
	user := req.Messages[2]
	if len(user.Content) != 3 {
		t.Fatalf("tool results = %d", len(user.Content))
	}
	for i, b := range user.Content {
		if b.Type != ir.BlockToolResult {
			t.Fatalf("block %d type = %v", i, b.Type)
		}
		if b.ToolResult.ToolUseID != callIDs[i] {
			t.Errorf("result %d paired to %q, want %q", i, b.ToolResult.ToolUseID, callIDs[i])
		}
	}
	// 同名按顺序配对：两个 get_weather 不能串号
	if got := user.Content[0].ToolResult.Content[0].Text; got != "sunny" {
		t.Errorf("result[0] text = %q", got)
	}
	// response 为任意 JSON object 时保留原文
	if got := user.Content[2].ToolResult.Content[0].Text; !strings.Contains(got, "price") {
		t.Errorf("result[2] text = %q, want raw JSON", got)
	}
}

// Gemini 客户端非流式响应仍可编码 functionCall，并补齐必须的 thoughtSignature。
func TestEncodeResponse_ToolCall(t *testing.T) {
	resp := &ir.Response{
		ID: "resp-1", Model: "gemini-2.5-pro", StopReason: ir.StopToolUse,
		Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
			ID: "call_1", Name: "get_weather", Input: []byte(`{"city":"Paris"}`),
		}}},
	}
	body, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	if !strings.Contains(out, `"functionCall":{"name":"get_weather"`) {
		t.Errorf("missing functionCall: %s", out)
	}
	if !strings.Contains(out, dummyThoughtSignature) {
		t.Errorf("missing dummy thoughtSignature: %s", out)
	}
}

// Gemini 客户端流式响应仍可编码 functionCall，并补齐必须的 thoughtSignature。
func TestStreamEncoder_ToolCall(t *testing.T) {
	enc := New().NewStreamEncoder()
	_, _ = enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "resp-1", Model: "gemini-2.5-pro"})
	_, _ = enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "call_1", Name: "get_weather"}}})
	_, _ = enc.Encode(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"city":"Paris"}`})
	frames, err := enc.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	out := string(frames[0])
	if !strings.Contains(out, `"functionCall":{"name":"get_weather"`) {
		t.Errorf("missing functionCall: %s", out)
	}
	if !strings.Contains(out, dummyThoughtSignature) {
		t.Errorf("missing dummy thoughtSignature: %s", out)
	}
}
