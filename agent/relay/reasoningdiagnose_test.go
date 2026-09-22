package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// reasoning 的三维子参数（summary/context/mode）只有 responses 一族有槽位。
// 其余出站丢弃必须报出（丢必报）：这三维都不影响 HTTP 状态码，客户端看不到
// 自己要求的摘要详略、上下文范围与推理模式被降级成了上游默认。
func TestDiagnoseReportsDroppedReasoningSubParams(t *testing.T) {
	req := &ir.Request{Thinking: &ir.ThinkingConfig{
		Enabled: true, Effort: ir.EffortHigh, Summary: "detailed",
		Context: json.RawMessage(`"all_turns"`), Mode: json.RawMessage(`"pro"`),
	}}
	for _, name := range []string{"openai-chat", "anthropic", "kiro"} {
		t.Run(name, func(t *testing.T) {
			notes := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
			for _, want := range []string{
				`summary preference "detailed"`, "reasoning context scope", "reasoning mode",
			} {
				if !strings.Contains(notes, want) {
					t.Errorf("缺诊断 %q：%s", want, notes)
				}
			}
		})
	}
	// 本族有槽位（codex 是 responses 的同形别名），不得误报。
	for _, name := range []string{"openai-responses", "codex"} {
		t.Run(name+"/silent", func(t *testing.T) {
			if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
				t.Errorf("%s 有 reasoning 子参数槽位，不该报：%v", name, notes)
			}
		})
	}
}

// 只丢一维就只报一维：没给过的维度不得凭空报出，否则说明里全是噪声，
// 读者无从判断哪一维真的被降级了。
func TestDiagnoseReasoningSubParamsOnlyReportWhatWasGiven(t *testing.T) {
	cases := []struct {
		name string
		tc   *ir.ThinkingConfig
		want []string
		gone []string
	}{
		{"summary-only", &ir.ThinkingConfig{Enabled: true, Effort: ir.EffortHigh, Summary: "concise"},
			[]string{`summary preference "concise"`}, []string{"context scope", "reasoning mode"}},
		{"context-only", &ir.ThinkingConfig{Enabled: true, Effort: ir.EffortHigh, Context: json.RawMessage(`"current_turn"`)},
			[]string{"reasoning context scope"}, []string{"summary preference", "reasoning mode"}},
		{"none-given", &ir.ThinkingConfig{Enabled: true, Effort: ir.EffortHigh},
			nil, []string{"summary preference", "context scope", "reasoning mode"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notes := strings.Join(Diagnose(&ir.Request{Thinking: c.tc}, "openai-chat", capsOf(t, "openai-chat")), "; ")
			for _, w := range c.want {
				if !strings.Contains(notes, w) {
					t.Errorf("缺诊断 %q：%s", w, notes)
				}
			}
			for _, g := range c.gone {
				if strings.Contains(notes, g) {
					t.Errorf("没给过的维度被误报 %q：%s", g, notes)
				}
			}
		})
	}
}
