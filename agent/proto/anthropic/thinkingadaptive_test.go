package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R63：anthropic 思考配置现代化。官方 SDK 核对（2026-09-22，messages.ts）：
// thinking.type 增 "adaptive"（"enabled" 已标废弃），thinking.display 取值
// summarized|omitted；output_config.effort 为封闭五值 low/medium/high/
// xhigh/max（OpenAI effort 七值的子集，无 none/minimal）。

func TestAdaptiveThinkingRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],
		"thinking":{"type":"adaptive","display":"omitted"}}`
	r, err := (codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.Thinking == nil || !r.Thinking.Enabled || !r.Thinking.Adaptive {
		t.Fatalf("adaptive 未进 IR：%+v", r.Thinking)
	}
	if r.Thinking.Display != "omitted" {
		t.Errorf("display = %q，want omitted", r.Thinking.Display)
	}
	if r.Thinking.BudgetTokens != 0 {
		t.Errorf("adaptive 不应带预算：%d", r.Thinking.BudgetTokens)
	}
	out, err := (codec{}).EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"thinking":{"type":"adaptive","display":"omitted"}`) {
		t.Errorf("adaptive+display 未回写: %s", s)
	}
	if strings.Contains(s, "budget_tokens") {
		t.Errorf("adaptive 不应发明预算: %s", s)
	}
}

func TestEnabledThinkingDisplayRoundTrip(t *testing.T) {
	body := `{"model":"m","max_tokens":9000,"messages":[{"role":"user","content":"hi"}],
		"thinking":{"type":"enabled","budget_tokens":5000,"display":"summarized"}}`
	r, err := (codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.Thinking == nil || r.Thinking.Adaptive || r.Thinking.BudgetTokens != 5000 {
		t.Fatalf("enabled 形态错位：%+v", r.Thinking)
	}
	out, err := (codec{}).EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"enabled"`) || !strings.Contains(s, `"display":"summarized"`) {
		t.Errorf("enabled+display 未回写: %s", s)
	}
}

func TestOutputConfigEffortRoundTrip(t *testing.T) {
	for _, lv := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],
			"thinking":{"type":"adaptive"},"output_config":{"effort":"` + lv + `"}}`
		r, err := (codec{}).DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s DecodeRequest: %v", lv, err)
		}
		if r.Thinking == nil || r.Thinking.Effort != lv {
			t.Fatalf("%s 未进 IR：%+v", lv, r.Thinking)
		}
		out, err := (codec{}).EncodeRequest(r)
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", lv, err)
		}
		if !strings.Contains(string(out), `"effort":"`+lv+`"`) {
			t.Errorf("%s 未回写: %s", lv, out)
		}
	}
}

// effort 独立出现（没带 thinking 块）也算开了思考，同族往返不丢。
func TestEffortOnlyImpliesThinking(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],
		"output_config":{"effort":"high"}}`
	r, err := (codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.Thinking == nil || !r.Thinking.Enabled || r.Thinking.Effort != "high" {
		t.Fatalf("effort-only 未开思考：%+v", r.Thinking)
	}
	out, err := (codec{}).EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"effort":"high"`) {
		t.Errorf("effort 未回写: %s", out)
	}
}

// 值集装不下的档位（OpenAI 的 minimal、未知值）出站不写，由诊断报出；
// none 与未开思考同义，同样不写但无需报。
func TestEffortOutOfSetDropped(t *testing.T) {
	for _, lv := range []string{"minimal", "none", "turbo"} {
		out, err := (codec{}).EncodeRequest(&ir.Request{
			Model: "m", MaxTokens: 10,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
			Thinking: &ir.ThinkingConfig{Enabled: true, Effort: lv},
		})
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", lv, err)
		}
		if strings.Contains(string(out), "effort") {
			t.Errorf("%q 越集仍写出: %s", lv, out)
		}
	}
}

// 全缺省时不多一个键：没有 thinking 没有 output_config。
func TestThinkingAbsentStaysAbsent(t *testing.T) {
	out, err := (codec{}).EncodeRequest(&ir.Request{
		Model: "m", MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "thinking") || strings.Contains(s, "output_config") {
		t.Errorf("缺省时发明思考配置: %s", s)
	}
}
