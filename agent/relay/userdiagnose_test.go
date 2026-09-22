package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// user 维度与 gemini 专属字段的诊断：kiro 装不下 user id 要报；
// safetySettings / cachedContent 没有任何出站接得住，四家恒报；
// 都没给时一条都不许出。

func TestDiagnoseUserIDDroppedOnlyOnKiro(t *testing.T) {
	req := &ir.Request{Metadata: map[string]string{"user_id": "u-1"}}
	got := strings.Join(Diagnose(req, "kiro", capsOf(t, "kiro")), "; ")
	if !strings.Contains(got, "user id") {
		t.Errorf("kiro 丢弃 user id 未报告：%q", got)
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 原生支持 user id，误报：%v", name, notes)
		}
	}
}

func TestDiagnoseSafetySettingsAlwaysReported(t *testing.T) {
	req := &ir.Request{SafetySettings: []ir.SafetySetting{
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "OFF"},
		{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_ONLY_HIGH"},
	}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "2 safety setting(s)") {
			t.Errorf("%s 丢弃 safetySettings 未报告：%q", name, got)
		}
	}
}

func TestDiagnoseCachedContentAlwaysReported(t *testing.T) {
	req := &ir.Request{CachedContent: "cachedContents/abc"}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "cached content") {
			t.Errorf("%s 丢弃 cachedContent 未报告：%q", name, got)
		}
	}
}

// 四维都没给时诊断必须全静默——误报会让客户端对正常请求起疑。
func TestDiagnoseUserAndGeminiFieldsSilentWhenAbsent(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(&ir.Request{}, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 空请求误报：%v", name, notes)
		}
	}
}
