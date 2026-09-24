package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// B5：gemini serviceTier 值集（flex/standard/priority）跨族映射。
// standard 是 gemini 方言的标准容量档，与 OpenAI default / anthropic
// standard_only 同义；flex/priority 三家都有恒通。
func TestGeminiServiceTierCrossFamily(t *testing.T) {
	for _, c := range []struct {
		tier, protoName, want string
		ok                    bool
	}{
		{"standard", "openai-chat", "default", true},
		{"standard", "openai-responses", "default", true},
		{"standard", "anthropic", "standard_only", true},
		{"flex", "openai-chat", "flex", true},
		{"priority", "openai-responses", "priority", true},
		// anthropic 请求值集只有 auto/standard_only，flex/priority 无等价。
		{"flex", "anthropic", "", false},
		{"priority", "anthropic", "", false},
	} {
		got, ok := proto.MapServiceTier(c.tier, c.protoName)
		if got != c.want || ok != c.ok {
			t.Errorf("MapServiceTier(%q, %q) = (%q, %v), want (%q, %v)",
				c.tier, c.protoName, got, ok, c.want, c.ok)
		}
	}

	// 端到端：gemini 入站的 flex 必须落到 chat 出站的 service_tier 上。
	req, err := proto.MustInbound("gemini").DecodeRequest([]byte(
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"serviceTier":"flex"}}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := proto.MustOutbound("openai-chat").EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"service_tier":"flex"`) {
		t.Errorf("gemini flex must reach chat wire: %s", out)
	}

	// standard 到 chat 必须译成 default，不得原样发明非法值。
	req2 := &ir.Request{Model: "m", ServiceTier: "standard",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out2, err := proto.MustOutbound("openai-chat").EncodeRequest(req2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), `"service_tier":"default"`) {
		t.Errorf("gemini standard must map to chat default: %s", out2)
	}
	if strings.Contains(string(out2), `"service_tier":"standard"`) {
		t.Errorf("invalid chat value invented: %s", out2)
	}
}
