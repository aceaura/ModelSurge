package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R90：涂抹思考块（anthropic redacted_thinking）不进 Kiro 载荷。密文只有
// Anthropic 能解，Kiro 载荷既没有承载不透明推理状态的槽位，也不能据此恢复思考链
// ——与 thinking 块同一处置（Kiro 无签名验证通道，重发必 400）。
// 损耗由 relay 诊断报出，这里只钉「不泄漏、不牵连同条消息的正文」。
func TestBuildPayload_RedactedThinkingSkipped(t *testing.T) {
	const cipher = "EmwKAhgBEgy3va3pzix/LafPsn4aDFIT2Xlxh0L5L8rLVyIwxtE3rAFBa8cwF4LHqJo="
	p := payloadOf(t, textReq(
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockRedactedThinking, RedactedData: cipher},
			{Type: ir.BlockText, Text: "answer"},
		}},
		ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go on"}}},
	), "claude-sonnet-4")
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), cipher) || strings.Contains(string(raw), "redacted_thinking") {
		t.Errorf("Kiro 载荷泄漏涂抹块：%s", raw)
	}
	h := historyOf(t, p)
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	asst, ok := h[1].(map[string]any)["assistantResponseMessage"].(map[string]any)
	if !ok {
		t.Fatalf("history[1] not assistant: %v", h[1])
	}
	if got, _ := asst["content"].(string); !strings.Contains(got, "answer") {
		t.Errorf("同一条消息的正文被误删：%q", got)
	}
}
