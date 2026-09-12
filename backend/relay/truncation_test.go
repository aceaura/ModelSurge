package relay

import (
	"strings"
	"testing"

	"relayd/backend/ir"
	"relayd/backend/proto"
)

func TestTruncationTracker_ToolNoticeOneShot(t *testing.T) {
	tr := NewTruncationTracker(true)
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
				ToolUseID: "toolu_1", Content: []ir.Block{{Type: ir.BlockText, Text: "orig"}},
			}},
		}},
	}}

	// 未记录：不修改
	if tr.InjectNotices(req) {
		t.Fatal("inject without record should be no-op")
	}

	tr.Record("up", []proto.TruncatedTool{{ID: "toolu_1", Name: "Write", Reason: "missing 2 closing brace(s)"}}, false, "")
	if !tr.InjectNotices(req) {
		t.Fatal("inject after record should modify")
	}
	got := req.Messages[0].Content[0].ToolResult.Content
	if len(got) != 2 || got[0].Type != ir.BlockText || !strings.Contains(got[0].Text, "[API Limitation]") {
		t.Fatalf("tool_result notice missing: %+v", got)
	}
	if !strings.Contains(got[0].Text, "Original tool result:") {
		t.Errorf("notice should retain original-result header: %q", got[0].Text)
	}
	if got[1].Text != "orig" {
		t.Errorf("original content must follow notice: %+v", got)
	}

	// 一次性：再次注入不命中
	req2 := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "toolu_1"}},
		}},
	}}
	if tr.InjectNotices(req2) {
		t.Error("second inject should not hit (one-shot)")
	}
}

func TestTruncationTracker_ContentNoticeOneShot(t *testing.T) {
	tr := NewTruncationTracker(true)
	truncated := "The model wrote a very long response that got cut off mid-s"
	next := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: truncated}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "continue"}}},
	}}

	tr.Record("up", nil, true, truncated)
	if !tr.InjectNotices(next) {
		t.Fatal("inject should add content notice")
	}
	if len(next.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (synthetic user inserted)", len(next.Messages))
	}
	synthetic := next.Messages[2]
	if synthetic.Role != ir.RoleUser || !strings.Contains(synthetic.Text(), "[System Notice]") {
		t.Fatalf("synthetic notice wrong: %+v", synthetic)
	}
	// 插在 assistant 之后、原 user 之前
	if next.Messages[3].Text() != "continue" {
		t.Errorf("notice inserted at wrong position: %+v", next.Messages)
	}

	// 一次性
	if tr.InjectNotices(next) {
		t.Error("second inject should not hit")
	}
}

func TestTruncationTracker_Disabled(t *testing.T) {
	tr := NewTruncationTracker(false)
	tr.Record("up", []proto.TruncatedTool{{ID: "t1"}}, true, "content")
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "content"}}},
	}}
	if tr.InjectNotices(req) {
		t.Error("disabled tracker must not inject")
	}
	if len(req.Messages) != 1 {
		t.Errorf("disabled tracker modified messages: %+v", req.Messages)
	}
}

func TestTruncationTracker_LRUEviction(t *testing.T) {
	tr := NewTruncationTracker(true)
	for i := 0; i < truncationMaxEntries+10; i++ {
		tr.Record("up", []proto.TruncatedTool{{ID: strings.Repeat("a", 200)[:200] + string(rune('0'+i%10)) + string(rune('0'+(i/10)%10)) + string(rune('0'+(i/100)%10))}}, false, "")
	}
	tr.mu.Lock()
	n := len(tr.entries)
	tr.mu.Unlock()
	if n > truncationMaxEntries {
		t.Errorf("entries = %d, want <= %d", n, truncationMaxEntries)
	}
}

func TestContentHashStable(t *testing.T) {
	long := strings.Repeat("x", 800)
	if contentHash(long) != contentHash(long[:750]) {
		t.Error("hash should only consider first 500 bytes")
	}
	if contentHash("a") == contentHash("b") {
		t.Error("different content hashed equal")
	}
}
