package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

func TestDiagnose(t *testing.T) {
	req := &ir.Request{
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm", Signature: "sig", SignatureFrom: "anthropic"}},
				{Type: ir.BlockText, Text: "ok"},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
		},
		Tools: []ir.Tool{{Hosted: ir.HostedWebSearch}},
	}

	full := proto.Capabilities{ThinkingSignature: true, Images: true, HostedTools: true, ThinkingForcedToolChoice: true}
	if notes := Diagnose(req, "anthropic", full); len(notes) != 0 {
		t.Errorf("same-protocol signature with full caps should produce no notes, got %v", notes)
	}

	chat := proto.Capabilities{ThinkingSignature: false, Images: true, HostedTools: false}
	notes := Diagnose(req, "openai-chat", chat)
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "1 thinking signature") {
		t.Errorf("missing signature note: %v", notes)
	}
	if !strings.Contains(joined, "web_search") {
		t.Errorf("missing hosted tool note: %v", notes)
	}

	// 跨族签名：上游支持签名回放但签名形态不同，会被 codec 降级
	notes = Diagnose(req, "gemini", full)
	joined = strings.Join(notes, "; ")
	if !strings.Contains(joined, "1 thinking signature") || !strings.Contains(joined, "gemini") {
		t.Errorf("missing cross-protocol signature note: %v", notes)
	}

	// 来源不明的签名同样保守降级
	unknown := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm", Signature: "sig"}},
		}},
	}}
	if notes := Diagnose(unknown, "anthropic", full); len(notes) != 1 || !strings.Contains(notes[0], "thinking signature") {
		t.Errorf("unknown-origin signature should be noted: %v", notes)
	}

	// 无映射托管种类：即使上游支持托管工具也要提示
	req2 := &ir.Request{Tools: []ir.Tool{{Hosted: "file_search"}}}
	notes = Diagnose(req2, "anthropic", full)
	if len(notes) != 1 || !strings.Contains(notes[0], "file_search") {
		t.Errorf("unmapped hosted kind not reported: %v", notes)
	}

	// 无签名 thinking 不报警
	req3 := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm"}}}},
	}}
	if notes := Diagnose(req3, "openai-chat", chat); len(notes) != 0 {
		t.Errorf("unsigned thinking should not be noted: %v", notes)
	}

	// 思考模式 + 强制 tool_choice：上游不支持时提示降级，支持的协议不误报
	req4 := &ir.Request{
		Thinking:   &ir.ThinkingConfig{Enabled: true, Effort: "high"},
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny},
	}
	if notes := Diagnose(req4, "openai-chat", chat); len(notes) != 1 || !strings.Contains(notes[0], "tool_choice") {
		t.Errorf("forced tool choice in thinking mode not reported: %v", notes)
	}
	if notes := Diagnose(req4, "anthropic", full); len(notes) != 0 {
		t.Errorf("anthropic supports forced tool choice with thinking, got %v", notes)
	}

	// 思考未开启或 tool_choice 非 forced 时无需降级提示
	off := &ir.Request{
		Thinking:   &ir.ThinkingConfig{Enabled: false},
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAny},
	}
	if notes := Diagnose(off, "openai-chat", chat); len(notes) != 0 {
		t.Errorf("thinking off should not trigger tool_choice note: %v", notes)
	}
	auto := &ir.Request{
		Thinking:   &ir.ThinkingConfig{Enabled: true},
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto},
	}
	if notes := Diagnose(auto, "openai-chat", chat); len(notes) != 0 {
		t.Errorf("tool_choice auto should not trigger note: %v", notes)
	}
}
