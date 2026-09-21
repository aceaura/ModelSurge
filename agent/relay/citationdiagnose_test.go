package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func citationReq(n int) *ir.Request {
	cs := make([]ir.Citation, 0, n)
	for i := 0; i < n; i++ {
		cs = append(cs, ir.Citation{URL: "https://w", Start: i, End: i + 1})
	}
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "天气"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴", Citations: cs}}},
	}}
}

func citationNote(notes []string) string {
	for _, n := range notes {
		if strings.Contains(n, "citation") {
			return n
		}
	}
	return ""
}

// 真实 Caps 下三家协议都有槽位、只有 kiro 没有，分支两侧同时覆盖。
func TestDiagnoseCitationPerProtocol(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"anthropic", false},
		{"openai-chat", false},
		{"openai-responses", false},
		{"kiro", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := citationNote(Diagnose(citationReq(1), c.name, capsOf(t, c.name)))
			if c.want {
				if got == "" {
					t.Fatalf("无槽位却没报引用丢失")
				}
				if !strings.Contains(got, "1 citation(s)") {
					t.Errorf("没报出条数，读者无法判断影响面：%q", got)
				}
				return
			}
			if got != "" {
				t.Errorf("有槽位却报了丢失：%q", got)
			}
		})
	}
}

// 条数要累计：只报「发生了」而不报条数，读者无法判断影响面。
func TestDiagnoseCitationCountsAll(t *testing.T) {
	got := citationNote(Diagnose(citationReq(3), "kiro", capsOf(t, "kiro")))
	if !strings.Contains(got, "3 citation(s)") {
		t.Errorf("条数没累计：%q", got)
	}
}

// 无引用不得留下这条说明：恒真的诊断等于没有诊断。
func TestDiagnoseNoCitationNoNote(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
	}}
	if got := citationNote(Diagnose(req, "kiro", capsOf(t, "kiro"))); got != "" {
		t.Errorf("无引用却报了丢失：%q", got)
	}
}
