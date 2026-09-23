package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R67：responses context_management 的跨族诊断（caps.ResponseChain 门控，
// 与会话链同一位——context_management 本就挂在 responses 一族）。

func TestDiagnoseContextMgmtDroppedOffResponses(t *testing.T) {
	th := 200000
	req := &ir.Request{ContextMgmt: []ir.ContextMgmtEntry{{Type: "compaction", CompactThreshold: &th}}}
	for _, name := range []string{"anthropic", "openai-chat"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped context_management") {
			t.Errorf("%s 应报 context_management 丢失：%q", name, got)
		}
	}
	// responses 一族（含 codex 同形别名）接得住，零误报。
	for _, name := range []string{"openai-responses", "codex"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 误报：%v", name, notes)
		}
	}
	// 缺席静默。
	req.ContextMgmt = nil
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 空请求误报：%v", name, notes)
		}
	}
}
