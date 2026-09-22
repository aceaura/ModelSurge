package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R58 service_tier / prompt_cache_key 诊断：无槽位（kiro）与有槽位但值集
// 装不下（anthropic 的 priority、chat 的 ultrafast）是两种措辞，读者动作
// 不同（前者认命，后者可以换档位值）。缓存键值不回显（客户端自选串）。

func TestDiagnoseServiceTierNoSlot(t *testing.T) {
	req := &ir.Request{ServiceTier: "auto"}
	got := strings.Join(Diagnose(req, "kiro", capsOf(t, "kiro")), "; ")
	if !strings.Contains(got, "no capacity tier field") {
		t.Errorf("kiro 丢弃 service tier 未报告：%q", got)
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s auto 全通，误报：%v", name, notes)
		}
	}
}

func TestDiagnoseServiceTierNoEquivalent(t *testing.T) {
	for _, c := range []struct{ tier, target string }{
		{"priority", "anthropic"},
		{"flex", "anthropic"},
		{"ultrafast", "openai-chat"},
	} {
		t.Run(c.tier+"->"+c.target, func(t *testing.T) {
			req := &ir.Request{ServiceTier: c.tier}
			got := strings.Join(Diagnose(req, c.target, capsOf(t, c.target)), "; ")
			if !strings.Contains(got, "tier set has no equivalent") {
				t.Errorf("无等价档位未报告：%q", got)
			}
			if !strings.Contains(got, `"`+c.tier+`"`) {
				t.Errorf("档位值应出现在措辞里（枚举非敏感）：%q", got)
			}
		})
	}
	// 可映射的互译档位不报
	for _, c := range []struct{ tier, target string }{
		{"default", "anthropic"},
		{"standard_only", "openai-chat"},
		{"standard_only", "openai-responses"},
		{"ultrafast", "openai-responses"},
		{"flex", "openai-responses"},
	} {
		req := &ir.Request{ServiceTier: c.tier}
		if notes := Diagnose(req, c.target, capsOf(t, c.target)); len(notes) != 0 {
			t.Errorf("%s->%s 可映射，误报：%v", c.tier, c.target, notes)
		}
	}
}

func TestDiagnosePromptCacheKeyDroppedOffOpenAI(t *testing.T) {
	req := &ir.Request{PromptCacheKey: "secret-ish-client-chosen-key"}
	for _, name := range []string{"anthropic", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "prompt cache key") {
			t.Errorf("%s 丢弃缓存键未报告：%q", name, got)
		}
		if strings.Contains(got, "secret-ish-client-chosen-key") {
			t.Errorf("%s 回显了客户端自选键值：%q", name, got)
		}
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住缓存键，误报：%v", name, notes)
		}
	}
}

// 两维全缺省四家静默。
func TestDiagnoseRoutingParamsSilentWhenAbsent(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(&ir.Request{}, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 空请求误报：%v", name, notes)
		}
	}
}
