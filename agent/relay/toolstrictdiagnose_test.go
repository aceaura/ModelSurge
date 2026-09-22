package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R61：工具 strict 诊断。三族（anthropic/chat/responses）有槽位静默，
// kiro 无槽位报数；nil（没给）不计数。

func TestDiagnoseToolStrictDroppedToKiro(t *testing.T) {
	tr, fa := true, false
	req := &ir.Request{Tools: []ir.Tool{
		{Name: "a", Strict: &tr},
		{Name: "b", Strict: &fa},
		{Name: "c"}, // nil 不计
	}}
	got := strings.Join(Diagnose(req, "kiro", capsOf(t, "kiro")), "; ")
	if !strings.Contains(got, "strict flag on 2 tool(s)") {
		t.Errorf("kiro 丢弃 strict 未报告：%q", got)
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住 strict，误报：%v", name, notes)
		}
	}
}
