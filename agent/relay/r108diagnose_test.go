package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R108-乙5 anthropic output_config.task_budget 跨族丢弃注记：只有
// anthropic 有槽位，其余三族必须报得出；同族与缺席静默。
func TestDiagnoseR108TaskBudgetCrossFamily(t *testing.T) {
	req := &ir.Request{TaskBudget: []byte(`{"type":"tokens","total":50000}`)}
	for _, name := range []string{"openai-chat", "openai-responses", "codex"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped task_budget") {
			t.Errorf("%s 应报 task_budget 丢弃：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 同族误报：%v", notes)
	}
}

// R108-乙7 responses typed tool_choice（Raw 不透明槽）跨族注记：
// responses/codex 同族原样回写不报；chat/anthropic 编不出对应形状必须报。
func TestDiagnoseR108TypedToolChoiceCrossFamily(t *testing.T) {
	req := &ir.Request{
		Tools:      []ir.Tool{{Name: "file_search", Hosted: "file_search", HostedType: "file_search"}},
		ToolChoice: &ir.ToolChoice{Raw: []byte(`{"type":"file_search"}`)},
	}
	for _, name := range []string{"openai-chat", "anthropic"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped typed tool_choice") {
			t.Errorf("%s 应报 typed tool_choice 丢弃：%q", name, got)
		}
	}
	// 托管 file_search 落到外族：工具本体也得报损耗。anthropic 支持托管工具
	// 但无此种类的跨族映射；chat 根本不能执行托管工具，走整丢注记。
	if got := strings.Join(Diagnose(req, "anthropic", capsOf(t, "anthropic")), "; "); !strings.Contains(got, "no cross-protocol mapping for hosted tool(s) file_search") {
		t.Errorf("anthropic 应报 hosted file_search 未映射：%q", got)
	}
	if got := strings.Join(Diagnose(req, "openai-chat", capsOf(t, "openai-chat")), "; "); !strings.Contains(got, "dropped hosted tool(s) file_search") {
		t.Errorf("openai-chat 应报 hosted file_search 整丢：%q", got)
	}
	for _, name := range []string{"openai-responses", "codex"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 同族误报：%v", name, notes)
		}
	}
	// 结构化 tool_choice（非 Raw）不得触发该注记。
	plain := &ir.Request{ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto}}
	for _, name := range []string{"openai-chat", "anthropic"} {
		got := strings.Join(Diagnose(plain, name, capsOf(t, name)), "; ")
		if strings.Contains(got, "typed tool_choice") {
			t.Errorf("%s 结构化 tool_choice 误报：%q", name, got)
		}
	}
}
