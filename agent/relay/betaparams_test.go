package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// B4：speed 与 mcp_servers 跨族丢弃必须报出；同族（anthropic）闭嘴。
// mcp_servers 含凭据，注记不得回显值本身。
func TestDiagnoseSpeedAndMCPServers(t *testing.T) {
	req := &ir.Request{
		Speed:      "fast",
		MCPServers: json.RawMessage(`[{"type":"url","name":"docs","url":"https://mcp.example.com","authorization_token":"secret-token"}]`),
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		},
	}
	joined := strings.Join(Diagnose(req, "openai-chat", capsOf(t, "openai-chat")), "; ")
	for _, want := range []string{"dropped speed", "dropped mcp_servers"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q note: %s", want, joined)
		}
	}
	if strings.Contains(joined, "secret-token") {
		t.Errorf("note must not echo credentials: %s", joined)
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("native family must stay silent: %v", notes)
	}
}

// B7：videoMetadata 与 partMetadata 跨族丢弃必须报出（gemini 只入不出，
// 任何目标族都没有槽位，恒报）。
func TestDiagnoseVideoAndPartMetadata(t *testing.T) {
	req := &ir.Request{
		PartMetaParts: 2,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockMedia, Media: &ir.Media{
					Kind: ir.MediaVideo, MediaType: "video/mp4", URL: "https://x/v.mp4",
					VideoMeta: json.RawMessage(`{"startOffset":"10s"}`)}},
				{Type: ir.BlockText, Text: "hi"},
			}},
		},
	}
	joined := strings.Join(Diagnose(req, "anthropic", capsOf(t, "anthropic")), "; ")
	for _, want := range []string{"dropped video metadata on 1 media part(s)", "dropped part metadata on 2 part(s)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q note: %s", want, joined)
		}
	}
	if strings.Contains(joined, "startOffset") {
		t.Errorf("note must not echo metadata content: %s", joined)
	}
}
