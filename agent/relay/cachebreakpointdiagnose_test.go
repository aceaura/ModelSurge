package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R60：缓存断点跨族诊断。断点（块级 cache_control + tools[].cache_control）
// 是 anthropic 专属维度，跨族丢失此前完全静默。报数不报值。

func TestDiagnoseCacheBreakpointsDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{
		System: []ir.Block{{Type: ir.BlockText, Text: "sys", CacheCtl: "ephemeral"}},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "a", CacheCtl: "ephemeral"},
			{Type: ir.BlockText, Text: "b"}, // 无断点不计数
		}}},
		Tools: []ir.Tool{{Name: "ping", CacheCtl: "ephemeral"}},
	}
	for _, name := range []string{"openai-chat", "openai-responses", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "3 cache breakpoint(s)") {
			t.Errorf("%s 应报 3 个断点丢失：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住断点，误报：%v", notes)
	}
}

// 无断点四家静默。
func TestDiagnoseCacheBreakpointsSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser,
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 无断点误报：%v", name, notes)
		}
	}
}
