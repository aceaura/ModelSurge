package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// 调参维度的线索值：出现在下发字节里才算真的送到上游。
const (
	sigPresencePenalty  = 0.25
	sigFrequencyPenalty = 0.5
	sigSeed             = 4242
	sigCandidates       = 3
	sigTopLogProbs      = 7
)

func fullSamplingRequest() *ir.Request {
	pp, fp := sigPresencePenalty, sigFrequencyPenalty
	seed, n, top := sigSeed, sigCandidates, sigTopLogProbs
	yes := true
	return &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages:         []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		PresencePenalty:  &pp,
		FrequencyPenalty: &fp,
		Seed:             &seed,
		Candidates:       &n,
		LogProbs:         &yes,
		TopLogProbs:      &top,
		LogitBias:        map[string]float64{"1234": -50},
	}
}

// 出站方向：能力位说有就必须真的出现在请求体里，说没有就不得出现。
// 声明与实际反着来比两者都假更糟：诊断会报「已送达」而上游什么都没收到。
func TestOutboundSamplingMatchesCaps(t *testing.T) {
	// 按 JSON 键名判断而不是按值：载荷里可能带随机 hex 会话 id，
	// 小整数线索值在里头恒能撞上，按值判断会得到假阳性。
	type probe struct {
		cap  func(proto.Capabilities) bool
		keys []string
	}
	probes := map[string]probe{
		"penalties": {func(c proto.Capabilities) bool { return c.Penalties },
			[]string{`"presence_penalty"`, `"frequency_penalty"`}},
		"seed":       {func(c proto.Capabilities) bool { return c.Seed }, []string{`"seed"`}},
		"candidates": {func(c proto.Capabilities) bool { return c.Candidates }, []string{`"n"`}},
		"logprobs":   {func(c proto.Capabilities) bool { return c.LogProbs }, []string{`"top_logprobs"`}},
		"logitbias":  {func(c proto.Capabilities) bool { return c.LogitBias }, []string{`"logit_bias"`}},
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		c, err := proto.GetOutbound(name)
		if err != nil {
			t.Fatalf("%s 未注册为出站 codec: %v", name, err)
		}
		body, err := c.EncodeRequest(fullSamplingRequest())
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", name, err)
		}
		s := string(body)
		for dim, p := range probes {
			want := p.cap(c.Caps())
			for _, key := range p.keys {
				got := strings.Contains(s, key)
				if got != want {
					t.Errorf("%s/%s: 线索 %q 出现=%v, 能力位=%v\n%s", name, dim, key, got, want, s)
				}
			}
		}
	}
}

// 入站方向：客户端给的调参值必须进 IR。进不去就等于整维在本服务里不存在，
// 无论出站能不能装下都永久丢失。
func TestInboundSamplingReachesIR(t *testing.T) {
	cases := map[string]string{
		"openai-chat": `{"model":"m","messages":[{"role":"user","content":"hi"}],
			"presence_penalty":0.25,"frequency_penalty":0.5,"seed":4242,"n":3,
			"logprobs":true,"top_logprobs":7,"logit_bias":{"1234":-50}}`,
		"gemini": `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"generationConfig":{"presencePenalty":0.25,"frequencyPenalty":0.5,"seed":4242,
			"candidateCount":3,"responseLogprobs":true,"logprobs":7}}`,
	}
	for name, body := range cases {
		c, err := proto.GetInbound(name)
		if err != nil {
			t.Fatalf("%s 未注册为入站 codec: %v", name, err)
		}
		req, err := c.DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s DecodeRequest: %v", name, err)
		}
		if req.PresencePenalty == nil || *req.PresencePenalty != sigPresencePenalty {
			t.Errorf("%s presence_penalty = %v", name, req.PresencePenalty)
		}
		if req.FrequencyPenalty == nil || *req.FrequencyPenalty != sigFrequencyPenalty {
			t.Errorf("%s frequency_penalty = %v", name, req.FrequencyPenalty)
		}
		if req.Seed == nil || *req.Seed != sigSeed {
			t.Errorf("%s seed = %v", name, req.Seed)
		}
		if req.Candidates == nil || *req.Candidates != sigCandidates {
			t.Errorf("%s candidates = %v", name, req.Candidates)
		}
		if req.LogProbs == nil || !*req.LogProbs {
			t.Errorf("%s logprobs = %v", name, req.LogProbs)
		}
		if req.TopLogProbs == nil || *req.TopLogProbs != sigTopLogProbs {
			t.Errorf("%s top_logprobs = %v", name, req.TopLogProbs)
		}
	}
	// logit_bias 只有 Chat 有这一维，单独断言：键是 token id，无跨协议翻译。
	c := proto.MustInbound("openai-chat")
	req, err := c.DecodeRequest([]byte(cases["openai-chat"]))
	if err != nil {
		t.Fatal(err)
	}
	if req.LogitBias["1234"] != -50 {
		t.Errorf("logit_bias = %v", req.LogitBias)
	}
}

// 零值不是「没给」：penalty 的 0 是不惩罚、seed 的 0 是一个具体种子、
// logprobs 的 false 是明确不要。用零值表达缺省就是替客户端表态。
func TestZeroValuesAreNotAbsent(t *testing.T) {
	zero, zeroSeed, no := 0.0, 0, false
	req := &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages:         []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		PresencePenalty:  &zero,
		FrequencyPenalty: &zero,
		Seed:             &zeroSeed,
		LogProbs:         &no,
	}
	c := proto.MustOutbound("openai-chat")
	body, err := c.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"presence_penalty", "frequency_penalty", "seed", "logprobs"} {
		if _, ok := got[k]; !ok {
			t.Errorf("显式零值 %s 被当成没给丢掉了：%s", k, body)
		}
	}
	// 反向：真的没给就一个字段都不能凭空出现（上游会把 penalty=0 当成一次表态）。
	bare, err := c.EncodeRequest(&ir.Request{Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var none map[string]json.RawMessage
	if err := json.Unmarshal(bare, &none); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"presence_penalty", "frequency_penalty", "seed", "n", "logprobs", "top_logprobs", "logit_bias"} {
		if _, ok := none[k]; ok {
			t.Errorf("客户端没给 %s，却凭空发了出去：%s", k, bare)
		}
	}
}

// Responses 没有独立的 logprobs 开关：客户端只给开关时须补一个档位，
// 否则目标协议明明支持、请求却落空。Chat 反向：只给档位不给开关，
// 上游同样什么都不算——两头都要钉（借档式映射必须两头都钉）。
func TestLogProbsSwitchAndLevelBridged(t *testing.T) {
	yes := true
	onlySwitch := &ir.Request{Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		LogProbs: &yes}
	c := proto.MustOutbound("openai-responses")
	body, err := c.EncodeRequest(onlySwitch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "top_logprobs") {
		t.Errorf("只给开关时没补档位，Responses 上游什么都不会算：%s", body)
	}

	// 客户端给了档位就必须原样送达。只断言键存在会被补档逻辑顶上：
	// 补的是 1，客户端要的是 7，上游按 1 算完全不是同一件事
	// （借档式映射必须两头都钉——键在与值对不能只钉一头）。
	withLevel := fullSamplingRequest()
	body, err = c.EncodeRequest(withLevel)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"top_logprobs":7`) {
		t.Errorf("客户端给的档位没原样送达：%s", body)
	}

	// Responses 入站只有档位：解出的 IR 必须同时带上开关，否则转去 Chat
	// 只带档位不带开关，Chat 上游也什么都不算。
	in := proto.MustInbound("openai-responses")
	req, err := in.DecodeRequest([]byte(`{"model":"m","input":"hi","top_logprobs":7}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.LogProbs == nil || !*req.LogProbs {
		t.Errorf("Responses 的 top_logprobs 兼任开关，IR 里没体现：%+v", req.LogProbs)
	}
	// 档位本身也要进 IR：只推出开关会让客户端要的 7 变成下游的默认值。
	if req.TopLogProbs == nil || *req.TopLogProbs != sigTopLogProbs {
		t.Errorf("档位没进 IR：%+v", req.TopLogProbs)
	}
	chat := proto.MustOutbound("openai-chat")
	out, err := chat.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"logprobs":true`) {
		t.Errorf("转去 Chat 时开关丢了：%s", out)
	}
}
