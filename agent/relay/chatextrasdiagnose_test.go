package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R68：chat 专属四维（modalities/audio/prediction/web_search_options）的
// 跨族诊断。四维 responses 全系也没有，protoName 直判仅 chat 自家静默。

func TestDiagnoseChatExtrasDroppedOffChat(t *testing.T) {
	req := &ir.Request{
		Modalities:       []string{"text", "audio"},
		AudioOut:         &ir.AudioOutParam{Format: "mp3", Voice: "alloy"},
		Prediction:       []byte(`{"type":"content","content":"x"}`),
		WebSearchOptions: []byte(`{"search_context_size":"high"}`),
	}
	for _, name := range []string{"anthropic", "openai-responses", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		for _, want := range []string{"dropped modalities/audio config", "dropped prediction config", "dropped web_search_options"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s 应报 %q：%q", name, want, got)
			}
		}
	}
	if notes := Diagnose(req, "openai-chat", capsOf(t, "openai-chat")); len(notes) != 0 {
		t.Errorf("chat 自家接得住，误报：%v", notes)
	}
}

// 只给 modalities 不给 audio（或反之）也报同一则：两维一体。
func TestDiagnoseChatExtrasPartial(t *testing.T) {
	for _, req := range []*ir.Request{
		{Modalities: []string{"audio"}},
		{AudioOut: &ir.AudioOutParam{Format: "wav", Voice: "echo"}},
	} {
		got := strings.Join(Diagnose(req, "anthropic", capsOf(t, "anthropic")), "; ")
		if !strings.Contains(got, "dropped modalities/audio config") {
			t.Errorf("部分给出也应报：%q", got)
		}
	}
}

func TestDiagnoseChatExtrasSilentWhenAbsent(t *testing.T) {
	req := &ir.Request{}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 空请求误报：%v", name, notes)
		}
	}
}
