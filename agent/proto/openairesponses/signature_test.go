package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 流式解码 reasoning item 的 encrypted_content 时必须带上来源形态，
// 否则聚合出的 thinking 在下一轮请求方向会被当成外族签名丢弃。
func TestStreamDecodeEncryptedContentCarriesOrigin(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("", `{"type":"response.output_item.done","output_index":0,
		"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc123"}}`)
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
	if sig.Text != "enc123" {
		t.Errorf("签名 = %q, want enc123", sig.Text)
	}
	if sig.SignatureFrom != Name {
		t.Errorf("来源 = %q, want %q", sig.SignatureFrom, Name)
	}
}

// 流式编码只把本族真签名放进 encrypted_content。外族/合成签名占了这一格，
// 客户端会把它当可回传的 reasoning 凭据，下一轮被上游拒。
func TestStreamEncodeEncryptedContentGatedByOrigin(t *testing.T) {
	encode := func(from string) string {
		enc := codec{}.NewStreamEncoder()
		var out []byte
		for _, ev := range []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt"},
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
	for _, from := range []string{"anthropic", "gemini", ir.SigSynthetic, ""} {
		s := encode(from)
		if !strings.Contains(s, "想") {
			t.Errorf("from=%q 思考正文被丢了：%s", from, s)
		}
		if strings.Contains(s, "SIGVALUE") {
			t.Errorf("from=%q 外来签名被写进 encrypted_content：%s", from, s)
		}
	}
	if s := encode(Name); !strings.Contains(s, "SIGVALUE") {
		t.Errorf("本族真签名没下发：%s", s)
	}
}

// 请求方向：签名装不下（外族/无签名）时 reasoning item 构造不出合法形态，
// 正文降级成 output_text 保住（anthropic degradeThinking 同款判据），签名位
// 不落线体；响应方向保留思考正文，只把签名位留空。
func TestReasoningItemDirectionalRules(t *testing.T) {
	msg := ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "想", Signature: "SIGVALUE", SignatureFrom: "gemini"}},
	}}
	items := encodeMessageItems(msg, true, nil)
	if len(items) != 1 || items[0].Type != "message" {
		t.Fatalf("请求方向外族签名的思考应降级成文本消息：%+v", items)
	}
	if !strings.Contains(string(items[0].Content), "想") {
		t.Errorf("降级后思考正文被丢了：%s", items[0].Content)
	}
	if strings.Contains(string(items[0].Content), "SIGVALUE") {
		t.Errorf("外族签名进了请求线体：%s", items[0].Content)
	}
	items = encodeMessageItems(msg, false, nil)
	if len(items) != 1 {
		t.Fatalf("响应方向应保留 reasoning 块：%+v", items)
	}
	if items[0].EncryptedContent != "" {
		t.Errorf("外族签名被写进 encrypted_content：%q", items[0].EncryptedContent)
	}
	if !strings.Contains(string(items[0].Summary), "想") {
		t.Errorf("思考正文被丢了：%s", items[0].Summary)
	}
	msg.Content[0].Thinking.SignatureFrom = Name
	for _, forRequest := range []bool{true, false} {
		items := encodeMessageItems(msg, forRequest, nil)
		if len(items) != 1 || items[0].EncryptedContent != "SIGVALUE" {
			t.Errorf("forRequest=%v 本族真签名没带上：%+v", forRequest, items)
		}
	}
}
