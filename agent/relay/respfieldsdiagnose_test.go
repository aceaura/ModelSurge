package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R56 responses 专属四维的诊断：conversation 锚点归 ResponseChain 位（responses
// 一族接得住），include/background/prompt 归 ResponsesExtras 位（其余三族全报）。
// 显式 background=false 与全缺省都必须静默——误报会让客户端对正常请求起疑。

func TestDiagnoseConversationDroppedOffChainProtocols(t *testing.T) {
	req := &ir.Request{ConversationID: "conv-1"}
	for _, name := range []string{"anthropic", "openai-chat", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "conversation anchor") {
			t.Errorf("%s 丢弃 conversation 锚点未报告：%q", name, got)
		}
	}
	// codex 与 responses 同能力，链锚点接得住
	for _, name := range []string{"openai-responses", "codex"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住 conversation，误报：%v", name, notes)
		}
	}
}

func TestDiagnoseBackgroundDroppedOffResponsesFamily(t *testing.T) {
	on := true
	req := &ir.Request{Background: &on}
	for _, name := range []string{"anthropic", "openai-chat", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "background mode") {
			t.Errorf("%s 丢弃 background 未报告：%q", name, got)
		}
	}
	for _, name := range []string{"openai-responses", "codex"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住 background，误报：%v", name, notes)
		}
	}
	// 显式 false 是客户端的选择不是能力诉求：同步响应正合其意，不该报
	off := false
	reqOff := &ir.Request{Background: &off}
	for _, name := range []string{"anthropic", "openai-chat", "kiro"} {
		if notes := Diagnose(reqOff, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s background=false 误报：%v", name, notes)
		}
	}
}

func TestDiagnoseIncludeDroppedOffResponsesFamily(t *testing.T) {
	req := &ir.Request{Include: []string{"message.output_text.logprobs", "code_interpreter_call.outputs"}}
	for _, name := range []string{"anthropic", "openai-chat", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "2 include value(s)") {
			t.Errorf("%s 丢弃 include 未报告：%q", name, got)
		}
	}
	for _, name := range []string{"openai-responses", "codex"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住 include，误报：%v", name, notes)
		}
	}
}

func TestDiagnosePromptRefDroppedOffResponsesFamily(t *testing.T) {
	req := &ir.Request{Prompt: &ir.PromptRef{ID: "pmpt-1", Version: "3"}}
	for _, name := range []string{"anthropic", "openai-chat", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "prompt template reference") {
			t.Errorf("%s 丢弃 prompt 模板引用未报告：%q", name, got)
		}
	}
	for _, name := range []string{"openai-responses", "codex"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住 prompt 引用，误报：%v", name, notes)
		}
	}
}

// 四维全缺省时四家都必须静默。
func TestDiagnoseResponsesExtrasSilentWhenAbsent(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(&ir.Request{}, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 空请求误报：%v", name, notes)
		}
	}
}
