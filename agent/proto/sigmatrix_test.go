package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// sigmatrix_test.go 思考签名来源（provenance）的跨协议矩阵。
//
// 签名是账号绑定的不透明凭据，只能在签发它的那个协议形态上回放。IR 用
// Thinking.SignatureFrom 记住来源，两个方向各有一条不可违反的规则：
//   - 请求方向（发给上游）：非本族签名不得回传，否则上游校验必拒整个请求。
//   - 响应方向（发给客户端）：非本族签名不得写进客户端的原生签名位，否则
//     客户端会把它当真凭据在下一轮回传，同样被拒——而且这次是它自己被拒。
//
// 第二条是本轮修的缺口：此前响应侧编码器一个都不看来源，外族签名与本代理
// 自己造的占位签名（kiro fake reasoning）都会被原样洗进原生签名位。

// sigSlotOf 各协议原生签名位的 JSON 键。空值表示该协议没有签名形态。
var sigSlotOf = map[string]string{
	"anthropic":        `"signature"`,
	"openai-responses": `"encrypted_content"`,
	"gemini":           `"thoughtSignature"`,
	"openai-chat":      "",
	"kiro":             `"signature"`,
}

func thinkingResponse(sig, from string) *ir.Response {
	return &ir.Response{
		ID: "msg_1", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
			Text: "思考正文", Signature: sig, SignatureFrom: from,
		}}},
	}
}

// 响应方向：每个有签名形态的入站协议，只在来源与自己同族时填原生签名位。
// 外族签名、占位签名、来源不明一律留空，而思考正文必须留下。
func TestResponseSignatureSlotGatedByOrigin(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-responses", "gemini"} {
		slot := sigSlotOf[name]
		c, err := proto.GetInbound(name)
		if err != nil {
			t.Fatalf("GetInbound(%s): %v", name, err)
		}
		for _, from := range []string{"anthropic", "openai-responses", "gemini", "kiro", ir.SigSynthetic, ""} {
			body, err := c.EncodeResponse(thinkingResponse("SIGVALUE", from))
			if err != nil {
				t.Fatalf("%s EncodeResponse(from=%s): %v", name, from, err)
			}
			s := string(body)
			if !strings.Contains(s, "思考正文") {
				t.Errorf("%s/from=%s：思考正文被丢了：%s", name, from, s)
			}
			got := strings.Contains(s, "SIGVALUE")
			if want := from == name; got != want {
				if want {
					t.Errorf("%s/from=%s：本族真签名没写进 %s：%s", name, from, slot, s)
				} else {
					t.Errorf("%s/from=%s：外来签名被洗进 %s：%s", name, from, slot, s)
				}
			}
		}
	}
}

// openai-chat 没有签名形态，任何来源的签名都不该出现在响应体里；
// 思考正文另有承载（reasoning_content），不得连正文一起丢。
func TestResponseSignatureAbsentOnChat(t *testing.T) {
	c, err := proto.GetInbound("openai-chat")
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"openai-chat", "anthropic", ir.SigSynthetic} {
		body, err := c.EncodeResponse(thinkingResponse("SIGVALUE", from))
		if err != nil {
			t.Fatal(err)
		}
		if s := string(body); strings.Contains(s, "SIGVALUE") {
			t.Errorf("chat/from=%s：无签名形态的协议吐出了签名：%s", from, s)
		} else if !strings.Contains(s, "思考正文") {
			t.Errorf("chat/from=%s：思考正文被丢了：%s", from, s)
		}
	}
}

func thinkingRequest(sig, from string) *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "思考正文", Signature: sig, SignatureFrom: from,
			}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "继续"}}},
		},
	}
}

// 请求方向：四个出站协议都只在同族时回传签名。这一条修复前就成立
// （anthropic degradeThinking / responses 跳过整块），这里把它钉住，
// 防止响应侧的门控被误加到请求侧、或请求侧门控被后续改动放宽。
func TestRequestSignatureReplayedOnlySameFamily(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-responses", "openai-chat", "kiro"} {
		c, err := proto.GetOutbound(name)
		if err != nil {
			t.Fatalf("GetOutbound(%s): %v", name, err)
		}
		for _, from := range []string{"anthropic", "openai-responses", "gemini", "kiro", ir.SigSynthetic, ""} {
			body, err := c.EncodeRequest(thinkingRequest("SIGVALUE", from))
			if err != nil {
				t.Fatalf("%s EncodeRequest(from=%s): %v", name, from, err)
			}
			s := string(body)
			// kiro 与 chat 无签名回放能力（Caps.ThinkingSignature=false），
			// 同族也不回传；其余协议仅同族回传。
			want := from == name && c.Caps().ThinkingSignature
			if got := strings.Contains(s, "SIGVALUE"); got != want {
				if want {
					t.Errorf("%s/from=%s：同族签名没回传，会话签名链断：%s", name, from, s)
				} else {
					t.Errorf("%s/from=%s：外来签名被回传给上游，请求会被拒：%s", name, from, s)
				}
			}
		}
	}
}

// 来源在流式路径上必须随事件带到聚合器：签名逐片到达，只有解码器知道来源。
// 丢了来源，同协议同上游的下一轮也会把自己刚发的签名当外族签名丢掉。
func TestStreamSignatureCarriesOrigin(t *testing.T) {
	agg := ir.NewAggregator()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		{Type: ir.EvThinkingDelta, Index: 0, Text: "想"},
		{Type: ir.EvSigDelta, Index: 0, Text: "sig-", SignatureFrom: "anthropic"},
		// 第二片不带来源（同一签名的续传），首片已定的来源不得被清掉。
		{Type: ir.EvSigDelta, Index: 0, Text: "tail"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	} {
		if !agg.Feed(ev) {
			break
		}
	}
	resp, err := agg.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Thinking == nil {
		t.Fatalf("聚合结果不是一个 thinking 块：%+v", resp.Content)
	}
	th := resp.Content[0].Thinking
	if th.Signature != "sig-tail" {
		t.Errorf("签名拼接错了：%q", th.Signature)
	}
	if th.SignatureFrom != "anthropic" {
		t.Errorf("来源没带过来（下一轮会被当外族丢掉）：%q", th.SignatureFrom)
	}
	if !th.SignatureGenuineFor("anthropic") {
		t.Error("同族判定为假")
	}
	if th.SignatureGenuineFor("gemini") {
		t.Error("跨族判定为真")
	}
}

// 合成来源与任何协议名都不相等：这是占位签名不被洗白的唯一依据。
func TestSyntheticOriginMatchesNoProtocol(t *testing.T) {
	th := &ir.Thinking{Signature: "sig_deadbeef", SignatureFrom: ir.SigSynthetic}
	for _, name := range []string{"anthropic", "openai-responses", "openai-chat", "gemini", "kiro"} {
		if th.SignatureGenuineFor(name) {
			t.Errorf("合成签名在 %s 上被判成真签名", name)
		}
	}
	// 空签名即使来源对得上也不是真签名，否则空串会被当凭据写进原生签名位。
	empty := &ir.Thinking{SignatureFrom: "anthropic"}
	if empty.SignatureGenuineFor("anthropic") {
		t.Error("空签名被判成真签名")
	}
}
