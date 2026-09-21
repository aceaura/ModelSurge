package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// Chat Completions 没有签名槽位：任何来源的签名都不得出现在下发字节里，
// 包括本族名——否则会凭空多出一个客户端无法解释的字段。思考正文照留。
func TestStreamEncodeNoSignatureSlot(t *testing.T) {
	for _, from := range []string{Name, "anthropic", "gemini", ir.SigSynthetic, ""} {
		enc := codec{}.NewStreamEncoder()
		var out []byte
		for _, ev := range []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "chatcmpl-1", Model: "gpt"},
			{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
			{Type: ir.EvThinkingDelta, Index: 0, Text: "想"},
			{Type: ir.EvSigDelta, Index: 0, Text: "SIGVALUE", SignatureFrom: from},
			{Type: ir.EvBlockStop, Index: 0},
		} {
			frames, err := enc.Encode(ev)
			if err != nil {
				t.Fatalf("encode %v: %v", ev.Type, err)
			}
			for _, fr := range frames {
				out = append(out, fr...)
			}
		}
		s := string(out)
		if !strings.Contains(s, "想") {
			t.Errorf("from=%q 思考正文被丢了：%s", from, s)
		}
		if strings.Contains(s, "SIGVALUE") {
			t.Errorf("from=%q 签名出现在无签名槽位的协议里：%s", from, s)
		}
	}
}
