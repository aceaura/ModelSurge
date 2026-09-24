package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 无签名思考块（chat 系 reasoning_content 跨族转入的常见形态）此前在请求
// 方向整块蒸发；现在降级成 output_text 文本，正文必须出现在线体里。
func TestEncodeRequestUnsignedThinkingDegradesToText(t *testing.T) {
	out, err := New().EncodeRequest(&ir.Request{
		Model: "gpt-x",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "先想三步"}},
				{Type: ir.BlockText, Text: "答"},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "先想三步") {
		t.Errorf("无签名思考正文被丢了：%s", s)
	}
	if strings.Contains(s, `"reasoning"`) {
		t.Errorf("无签名思考不该构造 reasoning item：%s", s)
	}
}

// 无签名且正文为空的思考块：什么可投递的都没有，不许编出空 output_text
// part（上游按 part 校验会拒空文本）。
func TestEncodeRequestEmptyUnsignedThinkingDropped(t *testing.T) {
	out, err := New().EncodeRequest(&ir.Request{
		Model: "gpt-x",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockThinking, Thinking: &ir.Thinking{}},
				{Type: ir.BlockText, Text: "答"},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"reasoning"`) || strings.Contains(s, `"text":""`) {
		t.Errorf("空思考块不该留下任何线体痕迹：%s", s)
	}
	if !strings.Contains(s, "答") {
		t.Errorf("相邻正文被牵连丢失：%s", s)
	}
}
