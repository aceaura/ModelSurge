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

// 真实 Caps 下四个出站都有引用槽位，所以有槽位那一侧是常态；缺槽位那一侧
// 只能翻能力位覆盖，否则这条诊断分支长期没人走过。
func TestDiagnoseCitationPerProtocol(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			if got := citationNote(Diagnose(citationReq(1), name, capsOf(t, name))); got != "" {
				t.Errorf("有槽位却报了丢失：%q", got)
			}
		})
		t.Run(name+"/无槽位", func(t *testing.T) {
			got := citationNote(Diagnose(citationReq(1), name, capsWithout(t, name, "Citations")))
			if got == "" {
				t.Fatal("无槽位却没报引用丢失")
			}
			if !strings.Contains(got, "1 citation(s)") {
				t.Errorf("没报出条数，读者无法判断影响面：%q", got)
			}
		})
	}
}

// 条数要累计：只报「发生了」而不报条数，读者无法判断影响面。
func TestDiagnoseCitationCountsAll(t *testing.T) {
	const base = "anthropic"
	got := citationNote(Diagnose(citationReq(3), base, capsWithout(t, base, "Citations")))
	if !strings.Contains(got, "3 citation(s)") {
		t.Errorf("条数没累计：%q", got)
	}
}

// 无引用不得留下这条说明：恒真的诊断等于没有诊断。
// 即便上游没有槽位也不报——没东西可丢。
func TestDiagnoseNoCitationNoNote(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
	}}
	const base = "anthropic"
	if got := citationNote(Diagnose(req, base, capsWithout(t, base, "Citations"))); got != "" {
		t.Errorf("无引用却报了丢失：%q", got)
	}
}

// 文档类引用是「有槽位但槽位装不下」：caps.Citations 是个整族布尔量，四个出站
// 全为真，靠它看不见这种逐条损耗。Anthropic 的 char_location / page_location /
// content_block_location 只有 document_index 与页/块/字符下标，而外族的标注槽位
// 以 URL 为来源身份——投给外族上游时这几条会被抹掉，必须单独数出来报。
func TestDiagnoseNonPortableCitations(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "天气"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴",
			Citations: []ir.Citation{
				{URL: "https://w", Start: 0, End: 2},
				{WireType: "char_location", CitedText: "晴", Start: 4, End: 5},
				{WireType: "page_location", CitedText: "晴"},
			}}}},
	}}
	for _, name := range []string{"codex", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			got := citationNote(Diagnose(req, name, capsOf(t, name)))
			if !strings.Contains(got, "dropped 2 document citation(s)") {
				t.Errorf("应报 2 条文档类引用丢失：%q", got)
			}
		})
	}
	// anthropic 五种形态都装得下，不报。
	t.Run("anthropic", func(t *testing.T) {
		if got := citationNote(Diagnose(req, "anthropic", capsOf(t, "anthropic"))); got != "" {
			t.Errorf("anthropic 误报：%q", got)
		}
	})
}
