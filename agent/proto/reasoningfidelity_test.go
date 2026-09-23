package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// reasoningfidelity_test.go 思考档位与 reasoning 子参数的线上保真。
//
// 判据是「客户端表达过的那一维，出站字节里必须还在，且不得被改写成别的值」。
// 两类失真此前都是 HTTP 200 且无任何说明：
//   - 显式 effort=none 被压成「省略」。省略不等于不思考：OpenAI 两族会按自己的
//     默认档（medium）思考，客户端要的「别思考」就变成了「中档思考」。
//   - reasoning 的子参数（summary/context/mode）在 IR 里没有位置，解码即丢；
//     summary 还被出站硬编码成 auto，把 detailed 降级了。
//
// 与 TestDisabledThinkingStaysOffAcrossOutbound 互为表里：那条钉住「关着不带
// 档位时不发明档位」，这里钉住「显式 none 必须写出去」。

const rfUserInput = `[{"type":"message","role":"user","content":"hi"}]`

func rfResponses(reasoning string) string {
	return `{"model":"m","reasoning":` + reasoning + `,"input":` + rfUserInput + `}`
}

func rfChat(effort string) string {
	return `{"model":"m","reasoning_effort":"` + effort +
		`","messages":[{"role":"user","content":"hi"}]}`
}

// rfConvert 按 relay 的真实顺序走：入站解码 -> CompleteThinking -> 出站编码。
func rfConvert(t *testing.T, inbound, body, outbound string) string {
	t.Helper()
	r, err := proto.MustInbound(inbound).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s DecodeRequest(%s): %v", inbound, body, err)
	}
	ir.CompleteThinking(r)
	out, err := proto.MustOutbound(outbound).EncodeRequest(r)
	if err != nil {
		t.Fatalf("%s EncodeRequest: %v", outbound, err)
	}
	return string(out)
}

// 客户端显式关思考时，线上必须看得见这个「关」。
func TestExplicitEffortNoneReachesOpenAIWire(t *testing.T) {
	cases := []struct{ inbound, body, outbound, want string }{
		{"openai-responses", rfResponses(`{"effort":"none"}`), "openai-responses", `"effort":"none"`},
		{"openai-responses", rfResponses(`{"effort":"none"}`), "openai-chat", `"reasoning_effort":"none"`},
		{"openai-responses", rfResponses(`{"effort":"none"}`), "codex", `"effort":"none"`},
		{"openai-chat", rfChat("none"), "openai-chat", `"reasoning_effort":"none"`},
		{"openai-chat", rfChat("none"), "openai-responses", `"effort":"none"`},
		{"openai-chat", rfChat("none"), "codex", `"effort":"none"`},
	}
	for _, c := range cases {
		t.Run(c.inbound+"->"+c.outbound, func(t *testing.T) {
			body := rfConvert(t, c.inbound, c.body, c.outbound)
			if !strings.Contains(body, c.want) {
				t.Errorf("显式 none 没到线上，缺 %s：%s", c.want, body)
			}
			// none 不产出 reasoning 项，还去点名要 encrypted_content 只是噪声。
			if strings.Contains(body, "encrypted_content") {
				t.Errorf("effort=none 仍在要思考签名：%s", body)
			}
		})
	}
}

// 同一个 vendor 档位值从两个入口进来必须读出同一套语义。此前 responses 把
// minimal 读成「不思考」而 chat 读成「思考」：同一个客户端值换个入口，出站
// 就从「最少思考」变成「不思考」，彻底掐掉客户端要的思考。
func TestSameEffortValueMeansSameThingFromEitherInbound(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "low", "medium", "high"} {
		t.Run(effort, func(t *testing.T) {
			resp, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(rfResponses(`{"effort":"` + effort + `"}`)))
			if err != nil {
				t.Fatalf("responses DecodeRequest: %v", err)
			}
			chat, err := proto.MustInbound("openai-chat").DecodeRequest([]byte(rfChat(effort)))
			if err != nil {
				t.Fatalf("chat DecodeRequest: %v", err)
			}
			if resp.Thinking == nil || chat.Thinking == nil {
				t.Fatalf("thinking 丢失：responses=%v chat=%v", resp.Thinking, chat.Thinking)
			}
			if resp.Thinking.Enabled != chat.Thinking.Enabled || resp.Thinking.Effort != chat.Thinking.Effort {
				t.Errorf("两族读出两套语义：responses Enabled=%v Effort=%q / chat Enabled=%v Effort=%q",
					resp.Thinking.Enabled, resp.Thinking.Effort, chat.Thinking.Enabled, chat.Thinking.Effort)
			}
		})
	}
	// minimal 是「最少的思考」，不是「不思考」。
	if th := decodeThinking(t, "openai-responses", rfResponses(`{"effort":"minimal"}`)); !th.Enabled {
		t.Errorf("minimal 被读成不思考：%+v", *th)
	}
	if th := decodeThinking(t, "openai-chat", rfChat("minimal")); !th.Enabled {
		t.Errorf("minimal 被读成不思考：%+v", *th)
	}
}

func decodeThinking(t *testing.T, inbound, body string) *ir.ThinkingConfig {
	t.Helper()
	r, err := proto.MustInbound(inbound).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s DecodeRequest: %v", inbound, err)
	}
	if r.Thinking == nil {
		t.Fatalf("%s 没解出 thinking：%s", inbound, body)
	}
	return r.Thinking
}

// minimal 是「最少的思考」，不是「不思考」：任何出站都不得把它写成 none。
// 三个有 effort 通道的出站还必须把档位原样落地（clamp 只许发生在越值集时，
// minimal 在 OpenAI 两系值集之内）。
func TestMinimalNeverBecomesSilenceOnAnyWire(t *testing.T) {
	bodies := []struct{ inbound, body string }{
		{"openai-responses", `{"model":"m","reasoning":{"effort":"minimal"},"input":` + rfUserInput + `}`},
		{"openai-chat", `{"model":"m","reasoning_effort":"minimal",` +
			`"messages":[{"role":"user","content":"hi"}]}`},
	}
	effortChannel := map[string]string{
		"openai-chat":      `"reasoning_effort":"minimal"`,
		"openai-responses": `"effort":"minimal"`,
		"codex":            `"effort":"minimal"`,
	}
	for _, c := range bodies {
		for _, outbound := range []string{"anthropic", "openai-chat", "openai-responses", "codex"} {
			t.Run(c.inbound+"->"+outbound, func(t *testing.T) {
				body := rfConvert(t, c.inbound, c.body, outbound)
				if strings.Contains(body, `"none"`) {
					t.Errorf("minimal 在线上变成了「不思考」：%s", body)
				}
				if want, ok := effortChannel[outbound]; ok && !strings.Contains(body, want) {
					t.Errorf("有 effort 通道的出站没落地 minimal，缺 %s：%s", want, body)
				}
			})
		}
	}
}

// reasoning 的四个子参数在同族往返里一个都不能少，且不得被改写。
func TestReasoningSubParamsSurviveSameFamilyRoundTrip(t *testing.T) {
	body := rfConvert(t, "openai-responses",
		rfResponses(`{"effort":"high","summary":"detailed","context":"all_turns","mode":"pro"}`),
		"openai-responses")
	for _, want := range []string{`"effort":"high"`, `"summary":"detailed"`, `"context":"all_turns"`, `"mode":"pro"`} {
		if !strings.Contains(body, want) {
			t.Errorf("缺 %s：%s", want, body)
		}
	}
	// 客户端给了详略偏好就不得改写成 auto：那是替客户端把摘要降级。
	if strings.Contains(body, `"summary":"auto"`) {
		t.Errorf("客户端的 detailed 被改写成 auto：%s", body)
	}
}

// 只给子参数不给档位时，整个 reasoning 对象此前会被丢掉（解码以 effort 非空为
// 前提）；补档位则是替客户端发明意图。
func TestReasoningWithoutEffortKeepsSubParamsAndInventsNoLevel(t *testing.T) {
	body := rfConvert(t, "openai-responses", rfResponses(`{"summary":"detailed"}`), "openai-responses")
	if !strings.Contains(body, `"summary":"detailed"`) {
		t.Fatalf("只给 summary 时整个 reasoning 被丢：%s", body)
	}
	if strings.Contains(body, `"effort"`) {
		t.Errorf("凭空补出档位：%s", body)
	}
	if th := decodeThinking(t, "openai-responses", rfResponses(`{"summary":"detailed"}`)); th.Enabled {
		t.Errorf("没表态档位却被读成开思考：%+v", *th)
	}
}

// 显式 null 等同没给：不归一的话 Clone 往返后变成非空 "null" 被写回线上。
func TestExplicitNullSubParamsStayAbsent(t *testing.T) {
	body := rfConvert(t, "openai-responses",
		rfResponses(`{"effort":"high","context":null,"mode":null}`), "openai-responses")
	for _, forbidden := range []string{`"context"`, `"mode"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("显式 null 被当成值写回线上 %s：%s", forbidden, body)
		}
	}
}

// 「关着」有两种：客户端显式关（Effort=none）必须写出去；关着却带着别的档位
// （账号覆盖强制关、anthropic disabled 配 output_config.effort）则一律不写——
// 写出去等于把「关」翻译成「开」。
func TestDisabledThinkingDistinguishesExplicitNoneFromStaleEffort(t *testing.T) {
	cases := []struct {
		effort string
		write  bool
	}{
		{ir.EffortNone, true},
		{ir.EffortHigh, false},
		{ir.EffortMinimal, false},
		{"", false},
	}
	keys := map[string]string{"openai-chat": `"reasoning_effort"`, "openai-responses": `"reasoning"`, "codex": `"reasoning"`}
	for _, c := range cases {
		for outbound, key := range keys {
			t.Run(c.effort+"/"+outbound, func(t *testing.T) {
				body := encodeWithCompletion(t, outbound,
					baseRequest(100000, &ir.ThinkingConfig{Enabled: false, Effort: c.effort}))
				if got := strings.Contains(body, key); got != c.write {
					t.Errorf("Enabled=false Effort=%q：%s 出现=%v，want %v：%s",
						c.effort, key, got, c.write, body)
				}
				if !c.write && strings.Contains(body, `"high"`) {
					t.Errorf("关着的思考被写成高档位：%s", body)
				}
			})
		}
	}
}

// 开了思考但没给档位时，本族仍需补一个默认档——那是 OpenAI 两族表达「要思考」
// 的唯一手段（没有独立的思考开关），不补就等于把开启意图丢了。
func TestEnabledWithoutEffortStillExpressesIntent(t *testing.T) {
	chat := encodeWithCompletion(t, "openai-chat",
		baseRequest(100000, &ir.ThinkingConfig{Enabled: true}))
	if !strings.Contains(chat, `"reasoning_effort":"medium"`) {
		t.Errorf("chat 没补出默认档：%s", chat)
	}
	resp := encodeWithCompletion(t, "openai-responses",
		baseRequest(100000, &ir.ThinkingConfig{Enabled: true}))
	if !strings.Contains(resp, `"effort":"medium"`) || !strings.Contains(resp, `"summary":"auto"`) {
		t.Errorf("responses 没补出默认档/摘要：%s", resp)
	}
	if !strings.Contains(resp, "reasoning.encrypted_content") {
		t.Errorf("开了思考却没要 encrypted_content，思考签名无从还原：%s", resp)
	}
}
