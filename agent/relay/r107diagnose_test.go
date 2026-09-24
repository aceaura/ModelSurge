package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R107：anthropic beta 三件套（context_management/diagnostics/user_profile_id）
// 跨族丢弃注记。与 responses 形态同位门控：只在目标是 anthropic 之外时发声。

func r107AnthropicBetaReq() *ir.Request {
	return &ir.Request{
		AnthropicContextMgmt: []byte(`{"edits":[{"type":"clear_tool_uses_20250919"}]}`),
		AnthropicDiagnostics: []byte(`{"previous_message_id":"msg_1"}`),
		UserProfileID:        "user-x",
	}
}

func TestDiagnoseAnthropicBetaDroppedCrossFamily(t *testing.T) {
	req := r107AnthropicBetaReq()
	for _, name := range []string{"openai-chat", "openai-responses", "codex"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		for _, want := range []string{
			"dropped context_management edits",
			"dropped diagnostics",
			"dropped user_profile_id",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s 应报 %q：%q", name, want, got)
			}
		}
	}
}

func TestDiagnoseAnthropicBetaSameFamilySilent(t *testing.T) {
	req := r107AnthropicBetaReq()
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 同族误报：%v", notes)
	}
}

// 三个字段各自缺席时不得发声（零值静默对照）。
func TestDiagnoseAnthropicBetaAbsentSilent(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(&ir.Request{}, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 空请求误报：%v", name, notes)
		}
	}
}
