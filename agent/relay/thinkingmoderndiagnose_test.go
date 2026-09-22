package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R63：anthropic 思考现代化三维的跨族/越集诊断。adaptive 与 display 是
// anthropic 专属，跨族报出；effort 越封闭五值集（minimal/未知值）在本族
// 报出，"none" 与缺省静默。

func TestDiagnoseAdaptiveThinkingDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Thinking: &ir.ThinkingConfig{
		Enabled: true, Adaptive: true, Display: "omitted"}}
	for _, name := range []string{"openai-chat", "openai-responses", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped adaptive thinking") {
			t.Errorf("%s 应报 adaptive 丢失：%q", name, got)
		}
		if !strings.Contains(got, "dropped thinking display preference") {
			t.Errorf("%s 应报 display 丢失：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

func TestDiagnoseAdaptiveSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 无 adaptive/display 误报：%v", name, notes)
		}
	}
}

func TestDiagnoseEffortValueSetOnAnthropic(t *testing.T) {
	mk := func(effort string) *ir.Request {
		return &ir.Request{Thinking: &ir.ThinkingConfig{Enabled: true, Effort: effort}}
	}
	got := strings.Join(Diagnose(mk("minimal"), "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(got, "dropped minimal thinking effort") {
		t.Errorf("minimal 越集未报：%q", got)
	}
	got = strings.Join(Diagnose(mk("turbo"), "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(got, `dropped thinking effort "turbo"`) {
		t.Errorf("未知值未带值报出：%q", got)
	}
	for _, lv := range []string{"", "none", "low", "medium", "high", "xhigh", "max"} {
		if notes := Diagnose(mk(lv), "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
			t.Errorf("effort %q 在集内误报：%v", lv, notes)
		}
	}
	// 值集诊断只管 anthropic 本族：其余协议 effort 直通或走自己的维度，不报。
	for _, name := range []string{"openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(mk("minimal"), name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 不应做 anthropic 值集诊断：%v", name, notes)
		}
	}
}
