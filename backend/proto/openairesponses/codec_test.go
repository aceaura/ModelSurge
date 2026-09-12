package openairesponses

import (
	"strings"
	"testing"

	"relayd/backend/ir"
)

// 请求方向：仅本族形态签名可还原 reasoning item（encrypted_content）；
// 外族/来源不明的签名跳过，避免构造上游无法解密的 item。
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
	}
}
