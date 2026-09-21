package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 请求方向解码：客户端回传的 thoughtSignature 认成本族真签名，
// 但我们自己塞的占位签名被回传时必须认成 synthetic——否则占位符被洗白，
// 会被当作有效凭据一路透传到上游。
func TestDecodeRequest_ThoughtSignatureOrigin(t *testing.T) {
	cases := []struct{ sig, wantFrom string }{
		{"realsig", Name},
		{dummyThoughtSignature, ir.SigSynthetic},
	}
	for _, c := range cases {
		body := `{"contents":[{"role":"model","parts":[{"text":"想","thought":true,"thoughtSignature":"` + c.sig + `"}]}]}`
		req, err := codec{}.DecodeRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		blk := findThinking(t, req)
		if blk.Thinking.Signature != c.sig {
			t.Errorf("签名 = %q, want %q", blk.Thinking.Signature, c.sig)
		}
		if blk.Thinking.SignatureFrom != c.wantFrom {
			t.Errorf("sig=%q 来源 = %q, want %q", c.sig, blk.Thinking.SignatureFrom, c.wantFrom)
		}
	}
}

// Gemini 原生把签名单独放一个 part：它属于前一个思考块。另起一块会让上游
// 多收到一个空 thinking，同时前一块的签名永久缺失。
func TestDecodeRequest_SignatureOnlyPartMergesIntoPrevThinking(t *testing.T) {
	body := `{"contents":[{"role":"model","parts":[
		{"text":"想","thought":true},
		{"thoughtSignature":"realsig"}]}]}`
	req, err := codec{}.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var thinks []ir.Block
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockThinking {
				thinks = append(thinks, b)
			}
		}
	}
	if len(thinks) != 1 {
		t.Fatalf("签名独立 part 应并进前一块，得到 %d 块：%+v", len(thinks), thinks)
	}
	if thinks[0].Thinking.Text != "想" || thinks[0].Thinking.Signature != "realsig" {
		t.Errorf("合并结果不对：%+v", thinks[0].Thinking)
	}
	if thinks[0].Thinking.SignatureFrom != Name {
		t.Errorf("来源 = %q, want %q", thinks[0].Thinking.SignatureFrom, Name)
	}
}

// ensureThoughtSignature 的判据落在 functionCall 自己那个 part 上：
// 校验是逐 part 做的，思考 part 上的真签名不能替 functionCall 顶账。
func TestEncodeResponse_FunctionCallGetsOwnSignature(t *testing.T) {
	out, err := codec{}.EncodeResponse(&ir.Response{
		ID: "r1", Model: "gemini-x",
		Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "想", Signature: "realsig", SignatureFrom: Name}},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "get_time", Input: []byte(`{}`)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"thoughtSignature":"realsig"`) {
		t.Errorf("思考块的本族真签名没带回：%s", s)
	}
	if !strings.Contains(s, dummyThoughtSignature) {
		t.Errorf("functionCall 的 part 缺签名占位：%s", s)
	}
}

// 响应方向签名位按来源门控：正文照留，外族/合成签名不得写进 thoughtSignature。
func TestEncodeResponse_ThoughtSignatureGatedByOrigin(t *testing.T) {
	enc := func(from string) string {
		out, err := codec{}.EncodeResponse(&ir.Response{
			ID: "r1", Model: "gemini-x",
			Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "想", Signature: "SIGVALUE", SignatureFrom: from,
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	for _, from := range []string{"anthropic", "kiro", ir.SigSynthetic, ""} {
		s := enc(from)
		if !strings.Contains(s, "想") {
			t.Errorf("from=%q 思考正文被丢了：%s", from, s)
		}
		if strings.Contains(s, "SIGVALUE") {
			t.Errorf("from=%q 外来签名被洗进 thoughtSignature：%s", from, s)
		}
	}
	if s := enc(Name); !strings.Contains(s, `"thoughtSignature":"SIGVALUE"`) {
		t.Errorf("本族真签名没带回：%s", s)
	}
}

// 流式编码同样门控：只有本族真签名才作为独立 part 下发。
func TestStreamEncodeThoughtSignatureGatedByOrigin(t *testing.T) {
	encode := func(from string) string {
		enc := codec{}.NewStreamEncoder()
		var out []byte
		for _, ev := range []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini-x"},
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
	for _, from := range []string{"anthropic", "openai-responses", ir.SigSynthetic, ""} {
		s := encode(from)
		if !strings.Contains(s, "想") {
			t.Errorf("from=%q 思考正文被丢了：%s", from, s)
		}
		if strings.Contains(s, "SIGVALUE") {
			t.Errorf("from=%q 外来签名被下发：%s", from, s)
		}
	}
	if s := encode(Name); !strings.Contains(s, "SIGVALUE") {
		t.Errorf("本族真签名没下发：%s", s)
	}
}

func findThinking(t *testing.T, req *ir.Request) ir.Block {
	t.Helper()
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockThinking && b.Thinking != nil {
				return b
			}
		}
	}
	t.Fatalf("没有 thinking 块：%+v", req.Messages)
	return ir.Block{}
}
