package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/upstream/ir"
)

// countTokens 文本级估算：空串 0、ASCII ≈ len/4、CJK 1 字 1 token、修正系数。
func TestCountTokens_Text(t *testing.T) {
	if got := countTokens("", true); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
	// 40 ASCII 字符 ≈ 10 token；×1.15 -> 11
	if got := countTokens(strings.Repeat("a", 40), true); got != 11 {
		t.Errorf("ascii 40 = %d, want 11 (10×1.15)", got)
	}
	if got := countTokens(strings.Repeat("a", 40), false); got != 10 {
		t.Errorf("ascii 40 no correction = %d, want 10", got)
	}
	// 20 个 CJK 字符 = 20 token；×1.15 -> 23
	if got := countTokens(strings.Repeat("中", 20), true); got != 23 {
		t.Errorf("cjk 20 = %d, want 23 (20×1.15)", got)
	}
}

// EstimateRequestTokens：消息 + 工具 + 系统分别计数，
// 服务 token（每消息 4、结尾 3、每工具 4）与修正系数齐备。
func TestEstimateRequestTokens(t *testing.T) {
	req := &ir.Request{
		Model:  "claude-sonnet-4-5",
		System: []ir.Block{{Type: ir.BlockText, Text: "sys prompt"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockText, Text: "hello world"},
				{Type: ir.BlockImage},
			}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
					ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`),
				}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					ToolUseID: "toolu_1",
					Content:   []ir.Block{{Type: ir.BlockText, Text: "file body"}},
				}},
			}},
		},
		Tools: []ir.Tool{{
			Name: "read_file", Description: "read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}
	got := (Codec{}).EstimateRequestTokens(req)
	// 手工推算（ASCII 4 字符/token、图片 100、每消息 4+role、结尾 3、每工具 4）：
	//   system "sys prompt"=2
	//   msg1 user: 4+1 + "hello world"=2 + image 100        = 107
	//   msg2 assistant: 4+2 + toolu_1=2+read_file=2+input=3  = 13
	//   msg3 user: 4+1 + toolu_1=2 + "file body"=2           = 9
	//   tool: 4 + read_file=2 + "read a file"=2 + schema=12  = 20
	//   结尾 3；合计 154，×1.15 = 177
	if got != 177 {
		t.Errorf("EstimateRequestTokens = %d, want 177", got)
	}
}

// 空请求也有最低开销（结尾 3 服务 token × 修正 ≈ 3），不至于估成大数。
func TestEstimateRequestTokens_Empty(t *testing.T) {
	got := (Codec{}).EstimateRequestTokens(&ir.Request{})
	if got < 3 || got > 4 {
		t.Errorf("empty request estimate = %d, want 3-4 (3 service tokens × 1.15)", got)
	}
}

// 未知块（server_tool_use 等）按 JSON 序列化兜底不丢量。
func TestCountBlockTokens_UnknownBlock(t *testing.T) {
	b := ir.Block{Type: ir.BlockServerToolUse,
		ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search", Input: json.RawMessage(`{"query":"go"}`)}}
	if got := countBlockTokens(b); got == 0 {
		t.Errorf("unknown block must fall back to JSON estimate, got 0")
	}
}
