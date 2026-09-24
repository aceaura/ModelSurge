package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R105：新增 IR 槽位的跨族损耗必须经 Diagnose 报出（R95 判据：「跳过/丢弃」
// 分支都要配一条注记断言）。同族不丢的维度，原生协议必须闭嘴。

func TestDiagnoseNotesDroppedResponsesExtras(t *testing.T) {
	three := 3
	no := false
	req := &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages:           []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Truncation:         "disabled",
		MaxToolCalls:       &three,
		IncludeObfuscation: &no,
	}
	notes := Diagnose(req, "anthropic", capsOf(t, "anthropic"))
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"dropped truncation", "dropped max_tool_calls", "dropped stream obfuscation"} {
		if !strings.Contains(joined, want) {
			t.Errorf("anthropic 出站没报 %q：%q", want, joined)
		}
	}
	// 原生形态闭嘴：responses 出站这三个维度全装得下，报一条都是谎报。
	for _, n := range Diagnose(req, "openai-responses", capsOf(t, "openai-responses")) {
		if strings.Contains(n, "truncation") || strings.Contains(n, "max_tool_calls") {
			t.Errorf("responses 原生维度被误报：%q", n)
		}
	}
	// 混淆开关 OpenAI 两系都有槽位，chat 出站同样不该报。
	for _, n := range Diagnose(req, "openai-chat", capsOf(t, "openai-chat")) {
		if strings.Contains(n, "obfuscation") {
			t.Errorf("chat 原生维度被误报：%q", n)
		}
	}
}

// gemini 专属声明键（labels/speechConfig/mediaResolution）恒报，值不建模只报键名。
func TestDiagnoseNotesGeminiExtras(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages:     []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		GeminiExtras: []string{"labels", "mediaResolution"},
	}
	notes := Diagnose(req, "openai-chat", capsOf(t, "openai-chat"))
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "labels") || !strings.Contains(joined, "mediaResolution") {
		t.Errorf("gemini 专属键没报出来：%q", joined)
	}
}

// gemini 来路的无映射托管工具（url_context 等）跨族出站丢弃要报出，
// 且不出现在出站请求体里。
func TestDiagnoseNotesUnmappedGeminiHostedTool(t *testing.T) {
	req := &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:    []ir.Tool{{Name: "url_context", Hosted: "url_context", HostedType: "urlContext"}},
	}
	notes := Diagnose(req, "anthropic", capsOf(t, "anthropic"))
	if !strings.Contains(strings.Join(notes, "\n"), "url_context") {
		t.Errorf("无映射托管工具没报出来：%q", notes)
	}
}
