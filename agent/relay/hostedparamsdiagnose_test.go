package relay

// R101 请求侧诊断：托管工具声明参数的跨族槽位。工具本体映射得过去
// （web_search 三族互达），但参数按目标族槽位取舍——max_uses 与
// blocked_domains 在 responses 没有槽位、search_context_size 在 anthropic
// 没有槽位。装不下不报，客户端要的限制静默失效。
//
// 同族静默：anthropic 从不会带来 search_context_size，responses 从不会带来
// max_uses，参数对象为空时更无从报——报出来就是谎报。chat 目标的托管工具
// 整丢已有注记，参数不重复报。

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func r101Req(p *ir.HostedParams) *ir.Request {
	return &ir.Request{
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:    []ir.Tool{{Name: "web_search", Hosted: ir.HostedWebSearch, HostedParams: p}},
	}
}

// anthropic 来源的 max_uses/blocked_domains 到 responses 无槽位，两个都报。
func TestDiagnoseReportsHostedParamsWithoutSlot(t *testing.T) {
	req := r101Req(&ir.HostedParams{
		MaxUses:        3,
		AllowedDomains: []string{"example.com"},
		BlockedDomains: []string{"spam.example"},
	})
	got := r100Notes(t, req, "openai-responses")
	if !strings.Contains(got, "max_uses") || !strings.Contains(got, "blocked_domains") {
		t.Errorf("无槽位参数没报出来：%s", got)
	}
	if strings.Contains(got, "allowed_domains") {
		t.Errorf("有槽直通的参数被误报：%s", got)
	}
}

// responses 来源的 search_context_size 到 anthropic 无槽位，要报。
func TestDiagnoseReportsSearchContextSizeOnAnthropic(t *testing.T) {
	req := r101Req(&ir.HostedParams{SearchContextSize: "high", AllowedDomains: []string{"example.com"}})
	got := r100Notes(t, req, "anthropic")
	if !strings.Contains(got, "search_context_size") {
		t.Errorf("search_context_size 无槽位没报出来：%s", got)
	}
	if strings.Contains(got, "allowed_domains") {
		t.Errorf("有槽直通的参数被误报：%s", got)
	}
}

// 同族静默：双方都有槽位的参数集合在两个目标上都不许出声。
func TestDiagnoseSilentWhenAllParamsHaveSlots(t *testing.T) {
	req := r101Req(&ir.HostedParams{MaxUses: 3, BlockedDomains: []string{"spam.example"}})
	if got := r100Notes(t, req, "anthropic"); strings.Contains(got, "hosted tool parameter") {
		t.Errorf("anthropic 目标同族参数被谎报：%s", got)
	}
	req2 := r101Req(&ir.HostedParams{SearchContextSize: "high"})
	if got := r100Notes(t, req2, "openai-responses"); strings.Contains(got, "hosted tool parameter") {
		t.Errorf("responses 目标同族参数被谎报：%s", got)
	}
	if got := r100Notes(t, r101Req(nil), "anthropic"); strings.Contains(got, "hosted tool parameter") {
		t.Errorf("nil 参数对象被谎报：%s", got)
	}
}

// chat 目标托管工具整丢已有注记，参数不得再报一遍。
func TestDiagnoseChatTargetDoesNotDoubleReportParams(t *testing.T) {
	req := r101Req(&ir.HostedParams{MaxUses: 3, BlockedDomains: []string{"spam.example"}})
	got := r100Notes(t, req, "openai-chat")
	if !strings.Contains(got, "dropped hosted tool(s)") {
		t.Errorf("chat 整丢注记丢了：%s", got)
	}
	if strings.Contains(got, "hosted tool parameter") {
		t.Errorf("整丢之外参数重复报：%s", got)
	}
}
