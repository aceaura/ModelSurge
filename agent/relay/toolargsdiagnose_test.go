package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func toolArgsReq(input string) *ir.Request {
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f", Input: json.RawMessage(input)}}}},
	}}
}

// 非法参数的诊断与槽位形态联动：对象槽位说「挪键」，字符串槽位说「原样透出」。
// 读者要改的地方不同，混在一起报等于没说。
func TestDiagnoseMalformedToolArgs(t *testing.T) {
	for _, name := range []string{"anthropic", "kiro"} {
		got := strings.Join(Diagnose(toolArgsReq(`{"city": "Par`), name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "rewrapped 1 tool call argument(s)") || !strings.Contains(got, ir.RawArgsKey) {
			t.Errorf("%s: %q", name, got)
		}
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		got := strings.Join(Diagnose(toolArgsReq(`{"city": "Par`), name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "passed through 1 malformed tool call argument(s)") {
			t.Errorf("%s: %q", name, got)
		}
	}
	// 非对象合法 JSON 同样算病态（对象槽位装不下，字符串槽位语义错位）。
	got := strings.Join(Diagnose(toolArgsReq(`[1,2]`), "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(got, "rewrapped") {
		t.Errorf("非对象参数未报: %q", got)
	}
}

// 合法参数与空参数都不是病态，诊断必须安静。
func TestDiagnoseToolArgsSilentWhenWellFormed(t *testing.T) {
	for _, in := range []string{`{"a":1}`, ``} {
		for _, name := range []string{"anthropic", "kiro", "openai-chat", "openai-responses"} {
			if notes := Diagnose(toolArgsReq(in), name, capsOf(t, name)); len(notes) != 0 {
				t.Errorf("in=%q %s: %v", in, name, notes)
			}
		}
	}
}
