// toolpolicy.go 严格 tool_choice 校验与一次恢复重发（KiroaaS
// streaming_core.py validate_tool_choice_result / add_tool_choice_recovery_directive
// / collect_with_tool_choice_retry 的 relay 侧翻译）。
// Kiro 上游无 tool_choice 字段，策略靠提示指令模拟（kiro.ToolChoiceDirective），
// 因此 none/any/named 需在网关侧缓冲校验；违规时向最后 user 消息追加
// 恢复指令原候选重发一次，再违规 502 tool_choice_not_satisfied。
package relay

import (
	"fmt"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto/kiro"
)

// strictToolChoice 返回请求的严格工具策略；nil = 非 strict（auto/未指定）。
func strictToolChoice(req *ir.Request) *ir.ToolChoice {
	if req == nil || req.ToolChoice == nil || req.ToolChoice.Mode == ir.ChoiceAuto {
		return nil
	}
	return req.ToolChoice
}

// toolViolation 一次校验违规（文案与参考实现逐字对齐）。
type toolViolation struct {
	msg string
}

func (v *toolViolation) Error() string { return v.msg }

func violf(format string, args ...any) *toolViolation {
	return &toolViolation{msg: fmt.Sprintf(format, args...)}
}

// responseToolNames 响应中的工具调用名（按 content 顺序）。
func responseToolNames(resp *ir.Response) []string {
	var names []string
	for _, b := range resp.Content {
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			names = append(names, b.ToolUse.Name)
		}
	}
	return names
}

// validateToolChoice 按策略校验聚合响应；violation = nil 表示通过。
func validateToolChoice(resp *ir.Response, policy *ir.ToolChoice) *toolViolation {
	names := responseToolNames(resp)
	for _, n := range names {
		if n == "" {
			return &toolViolation{msg: `response returned disallowed tool ""`}
		}
	}
	switch policy.Mode {
	case ir.ChoiceNone:
		if len(names) > 0 {
			return &toolViolation{msg: "tool_choice none returned one or more tools"}
		}
	case ir.ChoiceAny:
		if len(names) == 0 {
			return &toolViolation{msg: "tool_choice required returned no tools"}
		}
	case ir.ChoiceTool:
		if len(names) == 0 {
			return violf("required tool %q was not called", policy.ToolName)
		}
		for _, n := range names {
			if n != policy.ToolName {
				return violf("response called a tool other than required tool %q", policy.ToolName)
			}
		}
	}
	return nil
}

// appendRecoveryDirective 克隆请求并向最后一条 user 消息追加一次性恢复指令
// （"\n\n[Tool Policy Recovery] ... Retry THIS response now. {directive.strip}"）。
func appendRecoveryDirective(req *ir.Request, v *toolViolation, policy *ir.ToolChoice) *ir.Request {
	recovered := req.Clone()
	directive := strings.TrimSpace(kiro.ToolChoiceDirective(policy))
	text := fmt.Sprintf(
		"\n\n[Tool Policy Recovery] Your previous response violated the tool policy (%s). Retry THIS response now. %s",
		v.msg, directive,
	)
	for i := len(recovered.Messages) - 1; i >= 0; i-- {
		m := &recovered.Messages[i]
		if m.Role != ir.RoleUser {
			continue
		}
		// 追加到该消息最后一个文本块；无文本块时新起一块
		for j := len(m.Content) - 1; j >= 0; j-- {
			if m.Content[j].Type == ir.BlockText {
				m.Content[j].Text += text
				return recovered
			}
		}
		m.Content = append(m.Content, ir.Block{Type: ir.BlockText, Text: text})
		return recovered
	}
	// 无 user 消息（理论不可达）：整请求兜底追加一条
	recovered.Messages = append(recovered.Messages, ir.Message{
		Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: text}},
	})
	return recovered
}
