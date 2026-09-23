package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R61：工具 strict 诊断。三族（anthropic/chat/responses）有槽位静默，
// 无槽位报数；nil（没给）不计数。

func TestDiagnoseToolStrictDroppedWithoutSlot(t *testing.T) {
	tr, fa := true, false
	req := &ir.Request{Tools: []ir.Tool{
		{Name: "a", Strict: &tr},
		{Name: "b", Strict: &fa},
		{Name: "c"}, // nil 不计
	}}
	const base = "openai-chat"
	got := strings.Join(Diagnose(req, base, capsWithout(t, base, "ToolStrict")), "; ")
	if !strings.Contains(got, "strict flag on 2 tool(s)") {
		t.Errorf("无 strict 槽位时丢弃未报告：%q", got)
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住 strict，误报：%v", name, notes)
		}
	}
}
