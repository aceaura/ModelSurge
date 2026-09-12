package anthropic

import (
	"strings"
	"testing"

	"relayd/backend/ir"
)

func thinkingReq(sig, from string) *ir.Request {
	return &ir.Request{
		Model:     "claude-x",
		MaxTokens: 64,
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

// 解码时签名的协议形态标记为 anthropic。
func TestDecodeRequest_ThinkingSignatureFrom(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":64,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"hmm","signature":"sig"},
			{"type":"text","text":"ok"}]},
		{"role":"user","content":"go"}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	th := req.Messages[1].Content[0].Thinking
	if th.Signature != "sig" || th.SignatureFrom != "anthropic" {
		t.Errorf("thinking = %+v, want sig with SignatureFrom=anthropic", th)
	}
}

// 请求方向：同族签名透传；外族/无签名降级为 text——
// Anthropic 对历史 thinking 块强制签名校验，透传必 400。
func TestEncodeRequest_ThinkingDegradation(t *testing.T) {
	out, err := New().EncodeRequest(thinkingReq("sig", "anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"signature":"sig"`) {
		t.Errorf("same-protocol signature should pass through: %s", out)
	}

	for _, tc := range []struct{ sig, from string }{
		{"sig", "gemini"},  // 外族签名
		{"sig", ""},        // 来源不明的签名
		{"", "anthropic"},  // 无签名
	} {
		out, err := New().EncodeRequest(thinkingReq(tc.sig, tc.from))
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if strings.Contains(s, `"signature"`) {
			t.Errorf("signature (sig=%q from=%q) should be dropped: %s", tc.sig, tc.from, s)
		}
		if strings.Contains(s, `"thinking"`) {
			t.Errorf("unverifiable thinking block (sig=%q from=%q) should degrade to text: %s", tc.sig, tc.from, s)
		}
		if !strings.Contains(s, `"hmm"`) {
			t.Errorf("thinking text (sig=%q from=%q) should survive as text: %s", tc.sig, tc.from, s)
		}
	}
}

// 响应方向不降级：即使签名形态是外族，thinking 块也原样编码。
func TestEncodeResponse_ThinkingPassthrough(t *testing.T) {
	resp := &ir.Response{
		ID: "msg_1", Model: "claude-x",
		Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Text: "hmm", Signature: "sig", SignatureFrom: "gemini",
		}}},
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"thinking"`) || !strings.Contains(s, `"signature":"sig"`) {
		t.Errorf("response thinking must not be degraded: %s", s)
	}
}
