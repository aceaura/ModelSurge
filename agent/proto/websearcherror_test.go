package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// web_search 错误形态结果块（ErrorCode 非空）跨到 openai-responses 时没有
// 错误槽位——action.sources 装不下失败信号，损耗必须报出；同族（anthropic）
// 原生形态必须闭嘴。
func TestWebSearchErrorResultCrossFamilyNote(t *testing.T) {
	resp := &ir.Response{
		ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_1", Name: "web_search"}},
			{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{
				ToolUseID: "srvtoolu_1", ErrorCode: "max_uses_exceeded"}},
			{Type: ir.BlockText, Text: "done"},
		},
	}
	notes := proto.ScanResponseLosses(resp, "openai-responses", false, false, false)
	found := false
	for _, n := range notes {
		if strings.Contains(n, "web search result block") {
			found = true
		}
	}
	if !found {
		t.Errorf("errored web search result must be reported as dropped: %v", notes)
	}
	if notes := proto.ScanResponseLosses(resp, "anthropic", false, false, false); len(notes) != 0 {
		t.Errorf("native family must stay silent: %v", notes)
	}

	// 对照：正常结果数组形态与调用配对后可映射，不报损耗。
	resp.Content[1].WebSearchToolResult.ErrorCode = ""
	resp.Content[1].WebSearchToolResult.Results = []ir.WebSearchResult{{URL: "https://example.com"}}
	for _, n := range proto.ScanResponseLosses(resp, "openai-responses", false, false, false) {
		if strings.Contains(n, "web search result block") {
			t.Errorf("paired results form must not be reported: %v", n)
		}
	}
}
