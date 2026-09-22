package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R59 OpenAI 两系 2026 字段诊断：verbosity/moderation/prompt_cache_options
// 门控在 OpenAIExtras 位（只有 chat/responses/codex 有槽位）；
// safety_identifier 门控在 UserID 位——anthropic 有 UserID 位且能映进
// metadata.user_id，只有 user_id 槽被占时才丢。

func TestDiagnoseVerbosityDroppedOffOpenAI(t *testing.T) {
	req := &ir.Request{Verbosity: "low"}
	for _, name := range []string{"anthropic", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "verbosity") {
			t.Errorf("%s 丢弃 verbosity 未报告：%q", name, got)
		}
	}
	for _, name := range []string{"openai-chat", "openai-responses", "codex"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住 verbosity，误报：%v", name, notes)
		}
	}
}

func TestDiagnoseModerationDroppedOffOpenAI(t *testing.T) {
	req := &ir.Request{Moderation: json.RawMessage(`{"model":"omni-moderation-latest"}`)}
	for _, name := range []string{"anthropic", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "moderation") {
			t.Errorf("%s 丢弃 moderation 未报告：%q", name, got)
		}
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住 moderation，误报：%v", name, notes)
		}
	}
}

func TestDiagnosePromptCacheOptionsDroppedOffOpenAI(t *testing.T) {
	req := &ir.Request{PromptCacheOptions: json.RawMessage(`{"ttl":"30m"}`)}
	for _, name := range []string{"anthropic", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "prompt cache options") {
			t.Errorf("%s 丢弃缓存选项未报告：%q", name, got)
		}
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 接得住缓存选项，误报：%v", name, notes)
		}
	}
}

// safety_identifier：kiro 无 UserID 位恒丢；anthropic 能映进 metadata.user_id，
// 该槽被 user 占了才丢；OpenAI 两系原生接得住。值不回显。
func TestDiagnoseSafetyIdentifier(t *testing.T) {
	req := &ir.Request{SafetyIdentifier: "secret-sid-value"}
	got := strings.Join(Diagnose(req, "kiro", capsOf(t, "kiro")), "; ")
	if !strings.Contains(got, "safety identifier") {
		t.Errorf("kiro 丢弃 safety identifier 未报告：%q", got)
	}
	if strings.Contains(got, "secret-sid-value") {
		t.Errorf("回显了标识值：%q", got)
	}
	// anthropic：user_id 空着→映进去，不报
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 映得进，误报：%v", notes)
	}
	// anthropic：user_id 被占→丢，报「只有一个标识到达」
	req.Metadata = map[string]string{"user_id": "u1"}
	got = strings.Join(Diagnose(req, "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(got, "only one identifier") {
		t.Errorf("user_id 占位时的丢弃未报告：%q", got)
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		if notes := Diagnose(&ir.Request{SafetyIdentifier: "x"}, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 原生接得住，误报：%v", name, notes)
		}
	}
}
