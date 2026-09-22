package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/kiro"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R56 responses 入站剩余字段保真：include / conversation / background /
// prompt 模板是 Responses 一族的专属请求修饰，此前 DTO 只声明了 include
// 且从不读取，其余三个连声明都没有——客户端给的值静默蒸发。四维入 IR，
// 同协议（responses/codex）回写，其他三族诊断报出。

const (
	r56ConvClue   = "conv-r56-clue-8d3"
	r56PromptClue = "pmpt-r56-clue-5f1"
	r56IncClue    = "message.output_text.logprobs"
)

func r56Body() string {
	return `{"model":"m","input":"hi",` +
		`"include":["` + r56IncClue + `","reasoning.encrypted_content"],` +
		`"conversation":"` + r56ConvClue + `",` +
		`"background":true,` +
		`"prompt":{"id":"` + r56PromptClue + `","version":"3","variables":{"topic":"rust"}}}`
}

func TestResponsesExtrasDecodeIntoIR(t *testing.T) {
	r, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(r56Body()))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if len(r.Include) != 2 || r.Include[0] != r56IncClue {
		t.Errorf("Include = %v", r.Include)
	}
	if r.ConversationID != r56ConvClue {
		t.Errorf("ConversationID = %q", r.ConversationID)
	}
	if r.Background == nil || !*r.Background {
		t.Errorf("Background = %v", r.Background)
	}
	if r.Prompt == nil || r.Prompt.ID != r56PromptClue || r.Prompt.Version != "3" {
		t.Fatalf("Prompt = %+v", r.Prompt)
	}
	if !strings.Contains(string(r.Prompt.Variables), `"topic":"rust"`) {
		t.Errorf("Variables = %s", r.Prompt.Variables)
	}
}

// conversation 官方有两种形态：字符串 id 与 {id} 对象，都要归一成 id。
func TestConversationObjectFormDecoded(t *testing.T) {
	r, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(
		`{"model":"m","input":"hi","conversation":{"id":"` + r56ConvClue + `"}}`))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if r.ConversationID != r56ConvClue {
		t.Errorf("对象形态 ConversationID = %q", r.ConversationID)
	}
}

// 同协议出站（responses/codex）四维都要回写；客户端的 include 条目与
// 思考签名条目合并去重，谁的都不丢。
func TestResponsesExtrasRoundTripSameProtocol(t *testing.T) {
	r, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(r56Body()))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	// 思考开启时出站要加 reasoning.encrypted_content；客户端已点名，去重后只出现一次
	r.Thinking = nil // 先测不思考的纯透传
	for _, outName := range []string{"openai-responses", "codex"} {
		out, err := proto.MustOutbound(outName).EncodeRequest(r)
		if err != nil {
			t.Fatalf("%s: EncodeRequest err=%v", outName, err)
		}
		body := string(out)
		for _, clue := range []string{r56IncClue, r56ConvClue, r56PromptClue, `"background":true`, `"topic":"rust"`} {
			if !strings.Contains(body, clue) {
				t.Errorf("%s: 线索 %q 未回写: %s", outName, clue, body)
			}
		}
		// 客户端自己在 include 里点名了 encrypted_content，不开思考也应原样透传
		if !strings.Contains(body, "reasoning.encrypted_content") {
			t.Errorf("%s: 客户端点名的 include 条目被丢: %s", outName, body)
		}
	}
	// 开思考：客户端已列的 encrypted_content 不得重复
	r2, _ := proto.MustInbound("openai-responses").DecodeRequest([]byte(r56Body()))
	r2.Thinking = &ir.ThinkingConfig{Enabled: true, Effort: "medium"}
	out, _ := proto.MustOutbound("openai-responses").EncodeRequest(r2)
	if n := strings.Count(string(out), "reasoning.encrypted_content"); n != 1 {
		t.Errorf("encrypted_content 出现 %d 次，应去重为 1: %s", n, out)
	}
	if !strings.Contains(string(out), r56IncClue) {
		t.Errorf("客户端 include 条目被思考条目顶掉: %s", out)
	}
}

// 其他三族出站：四维一个字符都不进载荷。
func TestResponsesExtrasNeverLeakToOtherProtocols(t *testing.T) {
	r, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(r56Body()))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	for _, outName := range []string{"anthropic", "openai-chat", "kiro"} {
		out, err := proto.MustOutbound(outName).EncodeRequest(r)
		if err != nil {
			t.Fatalf("%s: EncodeRequest err=%v", outName, err)
		}
		body := string(out)
		for _, probe := range []string{r56ConvClue, r56PromptClue, r56IncClue,
			`"conversation":"`, `"background":`, `"prompt":{"id":`, `"include":`} {
			if strings.Contains(body, probe) {
				t.Errorf("%s: 泄漏 %q: %s", outName, probe, body)
			}
		}
	}
}

// 全缺省时：IR 零值，出站载荷不多一个键（缺席探针用键值对形态）。
func TestResponsesExtrasAbsentStayZero(t *testing.T) {
	r, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if len(r.Include) != 0 || r.ConversationID != "" || r.Background != nil || r.Prompt != nil {
		t.Errorf("缺省请求解出了东西：%+v", r)
	}
	out, err := proto.MustOutbound("openai-responses").EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest err=%v", err)
	}
	for _, probe := range []string{`"include":`, `"conversation":`, `"background":`, `"prompt":`} {
		if strings.Contains(string(out), probe) {
			t.Errorf("缺省时发明键 %q: %s", probe, out)
		}
	}
}
