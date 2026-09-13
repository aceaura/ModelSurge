// toolpolicy_test.go 严格工具策略校验与恢复指令的单测。
package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func textResp(blocks ...ir.Block) *ir.Response {
	return &ir.Response{Model: "m", Content: blocks, StopReason: ir.StopEndTurn}
}

// validateToolChoice 按策略判定违规；文案与 KiroaaS 参考实现逐字对齐。
func TestValidateToolChoice(t *testing.T) {
	toolUse := func(name string) ir.Block {
		return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "tu_1", Name: name, Input: json.RawMessage(`{}`)}}
	}
	text := func(s string) ir.Block { return ir.Block{Type: ir.BlockText, Text: s} }

	cases := []struct {
		name    string
		policy  *ir.ToolChoice
		blocks  []ir.Block
		wantVio string // "" = 通过
	}{
		{"none+无工具", &ir.ToolChoice{Mode: ir.ChoiceNone}, []ir.Block{text("hi")}, ""},
		{"none+带工具", &ir.ToolChoice{Mode: ir.ChoiceNone}, []ir.Block{toolUse("read_file")}, "tool_choice none returned one or more tools"},
		{"required+有工具", &ir.ToolChoice{Mode: ir.ChoiceAny}, []ir.Block{text("using"), toolUse("read_file")}, ""},
		{"required+纯文本", &ir.ToolChoice{Mode: ir.ChoiceAny}, []ir.Block{text("no tool")}, "tool_choice required returned no tools"},
		{"named+命中", &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "read_file"}, []ir.Block{toolUse("read_file")}, ""},
		{"named+未调用", &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "read_file"}, []ir.Block{text("ok")}, `required tool "read_file" was not called`},
		{"named+换工具", &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "read_file"}, []ir.Block{toolUse("write_file")}, `response called a tool other than required tool "read_file"`},
		{"named+混入其他", &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "read_file"}, []ir.Block{toolUse("read_file"), toolUse("write_file")}, `response called a tool other than required tool "read_file"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validateToolChoice(textResp(tc.blocks...), tc.policy)
			if tc.wantVio == "" {
				if v != nil {
					t.Fatalf("unexpected violation: %s", v.msg)
				}
				return
			}
			if v == nil || v.msg != tc.wantVio {
				t.Fatalf("violation = %v, want %q", v, tc.wantVio)
			}
		})
	}
}

// appendRecoveryDirective 克隆请求追加恢复指令：落到最后一条 user 消息，
// 原请求不被修改；指令含违规原因与去首尾空白的策略指令。
func TestAppendRecoveryDirective(t *testing.T) {
	policy := &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "read_file"}
	orig := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "use a tool"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "I'll just answer"}}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "again"},
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{{Type: ir.BlockText, Text: "r"}}}},
		}},
	}}
	clone := orig.Clone()
	v := violf("tool_choice required returned no tools")

	got := appendRecoveryDirective(clone, v, policy)
	if &got.Messages[0] == &clone.Messages[0] {
		t.Fatal("request not cloned")
	}
	// 前两条消息不受影响
	if got.Messages[0].Content[0].Text != "use a tool" || got.Messages[1].Content[0].Text != "I'll just answer" {
		t.Fatalf("earlier messages mutated: %+v", got.Messages)
	}
	// 指令追加到最后 user 消息最后一个文本块（toolResult 块之前）
	last := got.Messages[2]
	if last.Content[0].Text == "again" {
		t.Fatalf("directive not appended to last user message: %+v", last.Content)
	}
	if last.Content[1].Type != ir.BlockToolResult {
		t.Fatalf("toolResult block position changed: %+v", last.Content)
	}
	want := "\n\n[Tool Policy Recovery] Your previous response violated the tool policy " +
		"(tool_choice required returned no tools). Retry THIS response now. " +
		"[Tool Policy] For THIS response you MUST call the tool named 'read_file'. " +
		"Do not call any other tool and do not reply with text only."
	if !strings.HasSuffix(last.Content[0].Text, want) {
		t.Fatalf("directive text mismatch:\n got tail: %q\nwant tail: %q", last.Content[0].Text[len(last.Content[0].Text)-100:], want)
	}
	// 原请求未被污染
	if clone.Messages[2].Content[0].Text != "again" {
		t.Fatal("source request mutated")
	}
}

// 无文本块的 user 消息（纯 toolResult）：指令新起文本块。
func TestAppendRecoveryDirectiveNoTextBlock(t *testing.T) {
	policy := &ir.ToolChoice{Mode: ir.ChoiceAny}
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{{Type: ir.BlockText, Text: "r"}}}},
		}},
	}}
	got := appendRecoveryDirective(req, violf("tool_choice required returned no tools"), policy)
	last := got.Messages[0]
	if len(last.Content) != 2 || last.Content[1].Type != ir.BlockText ||
		!strings.Contains(last.Content[1].Text, "[Tool Policy Recovery]") {
		t.Fatalf("expected new text block with directive: %+v", last.Content)
	}
}

// strictToolChoice：auto/nil 非严格，none/any/named 严格。
func TestStrictToolChoice(t *testing.T) {
	if strictToolChoice(&ir.Request{}) != nil {
		t.Fatal("nil tool_choice should not be strict")
	}
	if strictToolChoice(&ir.Request{ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto}}) != nil {
		t.Fatal("auto should not be strict")
	}
	if strictToolChoice(&ir.Request{ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceNone}}) == nil {
		t.Fatal("none should be strict")
	}
}
