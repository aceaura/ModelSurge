package ir

import (
	"strings"
	"testing"
)

func compactMsgs() []Message {
	return []Message{
		{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "q1"}}},
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a1"}}},
		{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "q2"}}},
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a2"}}},
		{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "q3"}}},
	}
}

// K 轮切分：最后一条 user 永远保留；K=2 保留最后两个 user 边界轮。
func TestSplitForCompact(t *testing.T) {
	msgs := compactMsgs()
	if got := SplitForCompact(msgs, 2); got != 2 {
		t.Fatalf("k=2 keepFrom=%d, want 2 (q2 起)", got)
	}
	if got := SplitForCompact(msgs, 0); got != 4 {
		t.Fatalf("k=0 keepFrom=%d, want 4 (仅最后 user)", got)
	}
	if got := SplitForCompact(msgs, 99); got != 0 {
		t.Fatalf("k=99 keepFrom=%d, want 0 (全保留)", got)
	}
	if got := SplitForCompact(nil, 2); got != 0 {
		t.Fatalf("empty keepFrom=%d, want 0", got)
	}
	// 单条消息：无旧历史可压缩
	if got := SplitForCompact(msgs[4:], 2); got != 0 {
		t.Fatalf("single message keepFrom=%d, want 0", got)
	}
	// 无 user 消息：全部保留（不压缩）
	noUser := []Message{{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a"}}}}
	if got := SplitForCompact(noUser, 2); got != 0 {
		t.Fatalf("no user keepFrom=%d, want 0", got)
	}
}

// 工具配对边界回退：keep 首条为含 tool_result 的 user 且配对 tool_use
// 在更早的 assistant 时，边界回退到该 assistant（配对不切断）。
func TestSplitForCompactToolPairBoundary(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "q1"}}},
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "a1"}}},
		{Role: RoleAssistant, Content: []Block{{Type: BlockToolUse, ToolUse: &ToolUse{ID: "t1", Name: "get", Input: []byte(`{}`)}}}},
		{Role: RoleUser, Content: []Block{{Type: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "t1", Content: []Block{{Type: BlockText, Text: "r1"}}}}}},
		{Role: RoleAssistant, Content: []Block{{Type: BlockToolUse, ToolUse: &ToolUse{ID: "t2", Name: "get", Input: []byte(`{}`)}}}},
		{Role: RoleUser, Content: []Block{{Type: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "t2", Content: []Block{{Type: BlockText, Text: "r2"}}}}}},
	}
	// k=2：倒数第二个 user 边界是 t1 的 tool_result 载体 → 回退到含 t1
	// tool_use 的 assistant（下标 2）
	if got := SplitForCompact(msgs, 2); got != 2 {
		t.Fatalf("k=2 keepFrom=%d, want 2（回退到含 t1 配对的 assistant）", got)
	}
	// k=0：最后一条 user 是 t2 的 tool_result 载体 → 回退到 t2 的 assistant
	if got := SplitForCompact(msgs, 0); got != 4 {
		t.Fatalf("k=0 keepFrom=%d, want 4（回退到含 t2 tool_use 的 assistant）", got)
	}
}

// 压缩请求构造：单条 user 消息、max_tokens 4096、指令与旧历史渲染均在。
func TestBuildCompactRequest(t *testing.T) {
	old := compactMsgs()[:2]
	req := BuildCompactRequest(old)
	if req.MaxTokens != CompactMaxTokens || CompactMaxTokens != 4096 {
		t.Fatalf("max_tokens=%d, want 4096", req.MaxTokens)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != RoleUser {
		t.Fatalf("messages=%+v, want single user", req.Messages)
	}
	text := req.Messages[0].Text()
	for _, want := range []string{"Summarize", "conversation_to_summarize", "q1", "a1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("prompt missing %q: %s", want, text)
		}
	}
	// tool_use / tool_result 渲染
	oldTool := []Message{
		{Role: RoleAssistant, Content: []Block{{Type: BlockToolUse, ToolUse: &ToolUse{ID: "t1", Name: "get", Input: []byte(`{"a":1}`)}}}},
		{Role: RoleUser, Content: []Block{{Type: BlockToolResult, ToolResult: &ToolResult{ToolUseID: "t1", Content: []Block{{Type: BlockText, Text: "res"}}}}}},
	}
	req = BuildCompactRequest(oldTool)
	text = req.Messages[0].Text()
	if !strings.Contains(text, "tool_use get(t1)") || !strings.Contains(text, "tool_result t1: res") {
		t.Fatalf("tool rendering missing: %s", text)
	}
}

// 新历史构造：system/tools 保留；消息 = 合成 summary 消息 + keep 轮原文；
// 原始请求不被修改。
func TestBuildCompactedHistory(t *testing.T) {
	orig := &Request{
		Model:    "orig",
		System:   []Block{{Type: BlockText, Text: "sys"}},
		Tools:    []Tool{{Name: "get"}},
		Messages: compactMsgs(),
	}
	got := BuildCompactedHistory(orig, "SUMMARY", 2)
	if got.Model != "orig" || len(got.System) != 1 || len(got.Tools) != 1 {
		t.Fatalf("top-level fields lost: %+v", got)
	}
	if len(got.Messages) != 4 { // 合成 + q2 起 3 条
		t.Fatalf("messages=%d, want 4", len(got.Messages))
	}
	first := got.Messages[0]
	if first.Role != RoleUser || !strings.Contains(first.Text(), "SUMMARY") || !strings.Contains(first.Text(), "summary") {
		t.Fatalf("synthetic message = %+v", first)
	}
	if got.Messages[1].Text() != "q2" || got.Messages[3].Text() != "q3" {
		t.Fatalf("kept rounds wrong: %+v", got.Messages)
	}
	if len(orig.Messages) != 5 {
		t.Fatalf("original request mutated: %d messages", len(orig.Messages))
	}
}
