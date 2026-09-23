package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R62：anthropic 工具修饰四维跨族诊断。四维任一出现即计数该工具；
// anthropic 自家静默；全缺省四家静默。

func TestDiagnoseToolModifiersDroppedOffAnthropic(t *testing.T) {
	fa := false
	req := &ir.Request{Tools: []ir.Tool{
		{Name: "a", DeferLoading: true},
		{Name: "b", EagerInputStreaming: &fa},
		{Name: "c", InputExamples: []json.RawMessage{[]byte(`{}`)}},
		{Name: "d", AllowedCallers: []string{"direct"}},
		{Name: "e"}, // 无修饰不计
	}}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "tool modifiers on 4 tool(s)") {
			t.Errorf("%s 应报 4 件工具的修饰丢失：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

func TestDiagnoseToolModifiersSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{Tools: []ir.Tool{{Name: "plain"}}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 无修饰误报：%v", name, notes)
		}
	}
}
