package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 会话链三维的诊断：previous_response_id / store=true 只在无槽位的出站报；
// item_reference 恒报（代理无状态，同协议出站也复原不了）；都没给时静默。

func TestDiagnosePreviousResponseIDDroppedOffChain(t *testing.T) {
	req := &ir.Request{PreviousResponseID: "resp_1"}
	for _, name := range []string{"anthropic", "openai-chat", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "previous_response_id") {
			t.Errorf("%s 丢弃链锚点未报告：%q", name, got)
		}
	}
	for _, name := range []string{"openai-responses", "codex"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住链锚点，误报：%v", name, notes)
		}
	}
}

func TestDiagnoseStoreTrueDroppedOffChain(t *testing.T) {
	yes := true
	req := &ir.Request{Store: &yes}
	for _, name := range []string{"anthropic", "openai-chat", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "store=true") {
			t.Errorf("%s 丢弃 store=true 未报告：%q", name, got)
		}
	}
	// 显式 false 是「不要存」，没有任何东西被丢，不得误报。
	no := false
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(&ir.Request{Store: &no}, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 显式 store=false 误报：%v", name, notes)
		}
	}
}

func TestDiagnoseItemRefsAlwaysReported(t *testing.T) {
	req := &ir.Request{ItemRefs: 3}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "3 item reference(s)") {
			t.Errorf("%s 未报 item_reference 解析不了：%q", name, got)
		}
	}
}

// 三维都没给时诊断全静默。
func TestDiagnoseChainSilentWhenAbsent(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(&ir.Request{}, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 空请求误报：%v", name, notes)
		}
	}
}
