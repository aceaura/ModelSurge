package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 流式解码出的签名必须带上来源形态。流式是常态路径，来源丢了聚合出的
// thinking 就只有签名没有来源，下一轮请求方向会把自家真签名当外族丢掉。
func TestStreamDecodeSignatureCarriesOrigin(t *testing.T) {
	dec := New().NewStreamDecoder()
	for _, ev := range []struct{ event, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude"}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}`},
	} {
		if _, err := dec.Feed(ev.event, ev.data); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := dec.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"realsig"}}`)
	if err != nil {
		t.Fatal(err)
	}
	var sig *ir.Event
	for i := range evs {
		if evs[i].Type == ir.EvSigDelta {
			sig = &evs[i]
		}
	}
	if sig == nil {
		t.Fatalf("没有 EvSigDelta：%+v", evs)
	}
	if sig.Text != "realsig" {
		t.Errorf("签名 = %q, want realsig", sig.Text)
	}
	if sig.SignatureFrom != Name {
		t.Errorf("来源 = %q, want %q", sig.SignatureFrom, Name)
	}
}

// 流式编码只下发本族真签名。外族与合成签名进了 signature 位就是冒充，
// 客户端下一轮原样回传，Anthropic 的签名校验会拒掉整个请求。
func TestStreamEncodeSignatureGatedByOrigin(t *testing.T) {
	encode := func(from string) string {
		enc := New().NewStreamEncoder()
		var out []byte
		for _, ev := range []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "claude"},
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
		return string(out)
	}
	for _, from := range []string{"gemini", "openai-responses", ir.SigSynthetic, ""} {
		s := encode(from)
		if !strings.Contains(s, "想") {
			t.Errorf("from=%q 思考正文被丢了：%s", from, s)
		}
		if strings.Contains(s, "SIGVALUE") {
			t.Errorf("from=%q 外来签名被下发进签名位：%s", from, s)
		}
	}
	if s := encode(Name); !strings.Contains(s, "SIGVALUE") {
		t.Errorf("本族真签名没下发：%s", s)
	}
}
