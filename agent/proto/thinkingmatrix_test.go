package proto_test

import (
	"encoding/json"
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

// thinkingmatrix_test.go 推理风格跨协议换算的出站矩阵。
//
// 这一层验证的不是换算函数本身（ir/thinking_test.go 已穷举），而是
// 「客户端只表达了一种风格时，每个出站协议实际发出去的字节里那一维是不是
// 从客户端意图派生的」。这是本轮修的缺口所在：出站各自只读一侧，另一侧
// 缺失时会被硬编码缺省顶替，而两者都是 HTTP 200 且此前无任何说明。

// 换算落在 IR 层，所以矩阵的每一格都先过 CompleteThinking 再编码——与
// relay 的四条上游路径同序。
func encodeWithCompletion(t *testing.T, protoName string, req *ir.Request) string {
	t.Helper()
	c, err := proto.GetOutbound(protoName)
	if err != nil {
		t.Fatalf("GetOutbound(%s): %v", protoName, err)
	}
	ir.CompleteThinking(req)
	body, err := c.EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s EncodeRequest: %v", protoName, err)
	}
	return string(body)
}

func baseRequest(maxTokens int, tc *ir.ThinkingConfig) *ir.Request {
	return &ir.Request{
		Model:     "test-model",
		MaxTokens: maxTokens,
		Thinking:  tc,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
}

// 客户端只给 reasoning_effort（OpenAI 风格入站）时，Anthropic 系出站必须发出
// 从该档位派生的预算，而不是硬编码的 4096。
//
// 这是缺口一：proto/anthropic 的 EncodeRequest 只读 BudgetTokens，缺省补 4096。
// 客户端说 high（意为「多想」），100000 的 max_tokens 下应得 80000，
// 而修复前发出去的是 4096——差一个量级，且 HTTP 200 无任何说明。
func TestEffortOnlyReachesBudgetProtocols(t *testing.T) {
	cases := []struct {
		effort string
		want   string
	}{
		{ir.EffortLow, `"budget_tokens":20000`},
		{ir.EffortMedium, `"budget_tokens":50000`},
		{ir.EffortHigh, `"budget_tokens":80000`},
		{ir.EffortXHigh, `"budget_tokens":95000`},
	}
	for _, c := range cases {
		t.Run(c.effort, func(t *testing.T) {
			body := encodeWithCompletion(t, "anthropic",
				baseRequest(100000, &ir.ThinkingConfig{Enabled: true, Effort: c.effort}))
			if !strings.Contains(body, c.want) {
				t.Fatalf("anthropic body must carry %s (derived from effort %q), got: %s", c.want, c.effort, body)
			}
			// 钉住回归方向：硬编码缺省不得再出现。
			if strings.Contains(body, `"budget_tokens":4096`) {
				t.Fatalf("anthropic fell back to the hardcoded default instead of deriving from effort %q: %s", c.effort, body)
			}
		})
	}
}

// 客户端只给 thinking.budget_tokens（Anthropic 风格入站）时，OpenAI 系出站必须
// 发出从该预算派生的档位，而不是硬编码的 medium。
//
// 这是缺口二：openaichat/openairesponses 只读 Effort，缺省补 "medium"。
// 客户端给 32768（意为「多想」）修复前发出去的是 medium。
func TestBudgetOnlyReachesEffortProtocols(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{512, ir.EffortLow},
		{4096, ir.EffortMedium},
		{32768, ir.EffortHigh},
		{-1, ir.EffortHigh}, // 动态思考
	}
	for _, protoName := range []string{"openai-chat", "openai-responses", "codex"} {
		for _, c := range cases {
			t.Run(protoName+"/"+c.want, func(t *testing.T) {
				body := encodeWithCompletion(t, protoName,
					baseRequest(100000, &ir.ThinkingConfig{Enabled: true, BudgetTokens: c.budget}))
				var got struct {
					ReasoningEffort string `json:"reasoning_effort"`
					Reasoning       *struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatalf("unmarshal %s body: %v (%s)", protoName, err, body)
				}
				effort := got.ReasoningEffort
				if got.Reasoning != nil {
					effort = got.Reasoning.Effort
				}
				if effort != c.want {
					t.Fatalf("%s effort = %q, want %q (derived from budget %d): %s",
						protoName, effort, c.want, c.budget, body)
				}
			})
		}
	}
}

// 客户端只给 budget 时 kiro 出站的档位也必须来自统一换算，而不是它自己那套
// 与 new-api 不同源的阈值（>=8000 high / >=3000 medium / 其余 low）。
//
// 4096 在旧阈值下是 medium，在统一换算下也是 medium——所以这一格必须取一个
// 两套阈值判断不同的值：5000 旧阈值算 medium，统一换算（<=8192）也是 medium；
// 2000 旧阈值 low，统一换算（>1024）是 medium。取 2000 才能区分两者。
func TestBudgetOnlyReachesKiroEffort(t *testing.T) {
	// kiro 的 effort 通道按模型查表，需用有通道的模型名。
	req := baseRequest(100000, &ir.ThinkingConfig{Enabled: true, BudgetTokens: 2000})
	req.Model = "claude-sonnet-5"
	body := encodeWithCompletion(t, "kiro", req)
	if !strings.Contains(body, `"effort":"medium"`) {
		t.Fatalf("kiro effort must be derived by the shared conversion (budget 2000 -> medium), got: %s", body)
	}
	// 旧的就地阈值会把 2000 判成 low，这条断言钉住换算已收敛到一处。
	if strings.Contains(body, `"effort":"low"`) {
		t.Fatalf("kiro used its own legacy thresholds instead of the shared conversion: %s", body)
	}
}

// 两侧都给：客户端同时表达了两种风格，每个出站读自己那一侧、两侧都不得被改写。
func TestBothStatedStylesPassThroughUnchanged(t *testing.T) {
	const budget, effort = 60000, ir.EffortLow
	t.Run("anthropic keeps budget", func(t *testing.T) {
		body := encodeWithCompletion(t, "anthropic",
			baseRequest(100000, &ir.ThinkingConfig{Enabled: true, Effort: effort, BudgetTokens: budget}))
		if !strings.Contains(body, `"budget_tokens":60000`) {
			t.Fatalf("stated budget must survive: %s", body)
		}
	})
	t.Run("openai keeps effort", func(t *testing.T) {
		body := encodeWithCompletion(t, "openai-chat",
			baseRequest(100000, &ir.ThinkingConfig{Enabled: true, Effort: effort, BudgetTokens: budget}))
		if !strings.Contains(body, `"reasoning_effort":"low"`) {
			t.Fatalf("stated effort must survive (must not be re-derived from the budget): %s", body)
		}
	})
}

// 明确关闭推理时不得因为补全而带上推理配置。
// 补全后的值若泄进 wire body，关闭请求会被上游当成开启。
func TestDisabledThinkingStaysOffAcrossOutbound(t *testing.T) {
	for _, protoName := range []string{"anthropic", "openai-chat", "openai-responses", "codex"} {
		t.Run(protoName, func(t *testing.T) {
			body := encodeWithCompletion(t, protoName,
				baseRequest(100000, &ir.ThinkingConfig{Enabled: false, Effort: ir.EffortHigh, BudgetTokens: 32768}))
			for _, forbidden := range []string{"budget_tokens", "reasoning_effort", `"reasoning"`} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("%s must not carry %s when thinking is disabled: %s", protoName, forbidden, body)
				}
			}
		})
	}
}

// 未知档位不得被猜成某个预算：猜值会让本该只带 effort 的请求多出一个预算，
// 而 Anthropic 系在 budget_tokens 存在时会进入扩展思考模式并吞掉采样参数
// （cc-switch transform_codex_anthropic.rs:36-47 的理由）。
func TestUnknownEffortIsNotGuessedIntoBudget(t *testing.T) {
	body := encodeWithCompletion(t, "anthropic",
		baseRequest(100000, &ir.ThinkingConfig{Enabled: true, Effort: "ultra"}))
	// 无法换算时 anthropic 自己的缺省仍然生效（既有行为，不在本轮改动范围），
	// 但绝不能出现某个从 "ultra" 猜出来的百分比值。
	for _, guessed := range []string{"95000", "80000", "50000"} {
		if strings.Contains(body, guessed) {
			t.Fatalf("unknown effort must not be guessed into a budget, found %s: %s", guessed, body)
		}
	}
}
