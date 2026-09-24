package anthropic

import (
	"strings"
	"testing"
)

// B4：anthropic beta 请求参数 speed 与 mcp_servers 解码即丢的修复。
// 同族往返必须原样进出（含 mcp_servers 的凭据与嵌套 tool_configuration）。
func TestSpeedAndMCPServersRoundTrip(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":100,"speed":"fast",` +
		`"mcp_servers":[{"type":"url","name":"docs","url":"https://mcp.example.com/sse",` +
		`"authorization_token":"secret-token","tool_configuration":{"enabled":true,"allowed_tools":["search"]}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Speed != "fast" {
		t.Fatalf("speed = %q", req.Speed)
	}
	if len(req.MCPServers) == 0 {
		t.Fatal("mcp_servers dropped at decode")
	}

	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"speed":"fast"`,
		`"mcp_servers":[{"type":"url","name":"docs","url":"https://mcp.example.com/sse","authorization_token":"secret-token","tool_configuration":{"enabled":true,"allowed_tools":["search"]}}]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("re-encoded request missing %s: %s", want, s)
		}
	}

	// 没给这两个参数的请求不得发明键。
	plain, err := New().DecodeRequest([]byte(
		`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	pout, err := New().EncodeRequest(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pout), `"speed"`) || strings.Contains(string(pout), `"mcp_servers"`) {
		t.Errorf("absent params must not be invented: %s", pout)
	}
}
