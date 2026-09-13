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

// IR -> Gemini：functionResponse 的 name 由 tool_use id 反查回填；
// 含 functionCall 的 model content 无签名时注入占位 thoughtSignature。
func TestEncodeRequest_ToolRoundTrip(t *testing.T) {
	req := &ir.Request{
		Model: "gemini-2.5-pro",
		Tools: []ir.Tool{{Name: "get_weather"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "call_1", Name: "get_weather", Input: []byte(`{"city":"Paris"}`)}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "call_1", Content: []ir.Block{{Type: ir.BlockText, Text: "sunny"}}}},
			}},
		},
	}
	body, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	if !strings.Contains(out, `"functionCall":{"name":"get_weather"`) {
		t.Errorf("missing functionCall: %s", out)
	}
	if !strings.Contains(out, `"functionResponse":{"name":"get_weather"`) {
		t.Errorf("functionResponse name not resolved from id: %s", out)
	}
	if !strings.Contains(out, dummyThoughtSignature) {
		t.Errorf("missing dummy thoughtSignature: %s", out)
	}
}

// usage 换算：prompt 含 cached 需拆出，thoughts 计入 output。
func TestDecodeUsage(t *testing.T) {
	u := decodeUsage(&usageMetadata{
		PromptTokenCount: 100, CachedContentTokenCount: 30,
		CandidatesTokenCount: 10, ThoughtsTokenCount: 5,
	})
	if u.InputTokens != 70 || u.CacheReadTokens != 30 || u.OutputTokens != 15 {
		t.Errorf("usage = %+v", u)
	}
}

// 请求方向：同族 thoughtSignature 透传；外族形态签名置空防 400，
// thinking 文本仍作为 thought part 保留。
func TestEncodeRequest_ForeignSignatureStripped(t *testing.T) {
	mk := func(from string) *ir.Request {
		return &ir.Request{
			Model: "gemini-x",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
				{Role: ir.RoleAssistant, Content: []ir.Block{
					{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm", Signature: "sig", SignatureFrom: from}},
				}},
			},
		}
	}

	out, err := New().EncodeRequest(mk("gemini"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"thoughtSignature":"sig"`) {
		t.Errorf("same-protocol signature should pass through: %s", out)
	}

	out, err = New().EncodeRequest(mk("anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"thoughtSignature":"sig"`) {
		t.Errorf("foreign signature should be stripped: %s", s)
	}
	if !strings.Contains(s, "hmm") {
		t.Errorf("thinking text should survive as thought part: %s", s)
	}
}
