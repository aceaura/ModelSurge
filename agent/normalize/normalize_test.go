package normalize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func TestMergeAdjacentAndAlternating(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "a"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "b"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "c"}}},
	}}
	if err := Request(req, Strict()); err != nil {
		t.Fatal(err)
	}
	// 相邻同角色先合并成一条 user，交替已满足，无需再补位
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(req.Messages))
	}
	if got := req.Messages[0].Text(); got != "abc" {
		t.Fatalf("merged text = %q", got)
	}
}

func TestEnsureFirstUser(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
	}}
	if err := Request(req, Strict()); err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Role != ir.RoleUser {
		t.Fatalf("first role = %q, want user", req.Messages[0].Role)
	}
}

func TestEnsureAlternating(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "a"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "b"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "c"}}},
	}}
	// 只开交替步骤，观察补位行为
	if err := Request(req, Options{EnsureAlternating: true}); err != nil {
		t.Fatal(err)
	}
	want := []ir.Role{ir.RoleUser, ir.RoleAssistant, ir.RoleUser, ir.RoleAssistant}
	if len(req.Messages) != len(want) {
		t.Fatalf("messages = %d, want %d", len(req.Messages), len(want))
	}
	for i, m := range req.Messages {
		if m.Role != want[i] {
			t.Fatalf("role[%d] = %q, want %q", i, m.Role, want[i])
		}
	}
}

func TestFillEmptyContent(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser}}}
	if err := Request(req, Strict()); err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Text() == "" {
		t.Fatal("empty content not filled")
	}
}

func TestOrphanToolResultDowngraded(t *testing.T) {
	req := &ir.Request{
		Tools: []ir.Tool{{Name: "f", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "x", Content: []ir.Block{{Type: ir.BlockText, Text: "res"}}}},
			}},
		},
	}
	if err := Request(req, Strict()); err != nil {
		t.Fatal(err)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolResult {
				t.Fatal("orphan tool_result survived")
			}
		}
	}
}

// tool_use 后缺 tool_result 时补占位结果（Anthropic 硬性约束）。
func TestRequireToolPairing(t *testing.T) {
	req := &ir.Request{
		Tools: []ir.Tool{{Name: "f"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f", Input: json.RawMessage(`{}`)}},
			}},
		},
	}
	if err := Request(req, Strict()); err != nil {
		t.Fatal(err)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != ir.RoleUser || len(last.Content) == 0 || last.Content[0].Type != ir.BlockToolResult {
		t.Fatalf("missing placeholder tool_result: %+v", last)
	}
	if last.Content[0].ToolResult.ToolUseID != "c1" {
		t.Fatalf("placeholder paired to %q", last.Content[0].ToolResult.ToolUseID)
	}
}

// 并行 tool_result 分两条 user 消息到达时不得误插占位（合并先于配对）。
func TestRequireToolPairingParallelNoPlaceholder(t *testing.T) {
	tr := func(id string) ir.Message {
		return ir.Message{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: id, Content: []ir.Block{{Type: ir.BlockText, Text: "r"}}}},
		}}
	}
	req := &ir.Request{
		Tools: []ir.Tool{{Name: "f"}},
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f", Input: json.RawMessage(`{}`)}},
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c2", Name: "f", Input: json.RawMessage(`{}`)}},
			}},
			tr("c1"),
			tr("c2"),
		},
	}
	if err := Request(req, Options{MergeAdjacentRoles: true, RequireToolPairing: true, FixOrphanToolResults: true}); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (no placeholder): %+v", len(req.Messages), req.Messages)
	}
	for _, b := range req.Messages[1].Content {
		if b.Type != ir.BlockToolResult || b.ToolResult == nil {
			continue
		}
		for _, c := range b.ToolResult.Content {
			if c.Type == ir.BlockText && c.Text == Placeholder {
				t.Fatal("placeholder inserted for existing parallel result")
			}
		}
	}
}

func TestStripToolTracesWhenNoTools(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f", Input: json.RawMessage(`{}`)}},
		}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "c1", Content: []ir.Block{{Type: ir.BlockText, Text: "r"}}}},
		}},
	}}
	if err := Request(req, Strict()); err != nil {
		t.Fatal(err)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse || b.Type == ir.BlockToolResult {
				t.Fatal("tool trace survived without tools")
			}
		}
	}
}

func TestSanitizeSchema(t *testing.T) {
	in := json.RawMessage(`{"type":"object","required":[],"properties":{"a":{"type":"string","additionalProperties":false}},"additionalProperties":false}`)
	out := SanitizeSchema(in)
	if !json.Valid(out) {
		t.Fatalf("invalid json: %s", out)
	}
	s := string(out)
	// 空 required 数组照旧清掉（语义等同缺省）。
	if strings.Contains(s, `"required":[]`) {
		t.Errorf("empty required not sanitized: %s", s)
	}
	// additionalProperties 是契约的一部分：OpenAI strict 工具官方要求每个
	// object 节点都带 additionalProperties:false，剥掉后上游必 400。
	if got := strings.Count(s, `"additionalProperties":false`); got != 2 {
		t.Errorf("additionalProperties must be preserved at both levels, got %d: %s", got, s)
	}
}

func TestToolNameTooLong(t *testing.T) {
	req := &ir.Request{Tools: []ir.Tool{{Name: strings.Repeat("x", 65)}}}
	err := Request(req, Options{MaxToolNameLength: 64})
	if err == nil {
		t.Fatal("want error for long tool name")
	}
}
