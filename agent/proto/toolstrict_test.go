package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R61：工具 strict（schema 严格校验保证）三族贯通。官方 SDK 核对
// （2026-09-22）：anthropic Tool.strict、OpenAI chat FunctionDefinition.strict、
// responses FunctionTool.strict 同义同形。
// 三态指针：显式 false 与没给语义不同，都必须保真。

func boolPtr(b bool) *bool { return &b }

func TestToolStrictDecodeThreeFamilies(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"anthropic", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"name":"ping","input_schema":{"type":"object"},"strict":true},` +
			`{"name":"pong","input_schema":{"type":"object"},"strict":false},` +
			`{"name":"plain","input_schema":{"type":"object"}}]}`},
		{"openai-chat", `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"tools":[{"type":"function","function":{"name":"ping","strict":true}},` +
			`{"type":"function","function":{"name":"pong","strict":false}},` +
			`{"type":"function","function":{"name":"plain"}}]}`},
		{"openai-responses", `{"model":"m","input":"hi",` +
			`"tools":[{"type":"function","name":"ping","strict":true},` +
			`{"type":"function","name":"pong","strict":false},` +
			`{"type":"function","name":"plain"}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := proto.MustInbound(c.name).DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if len(r.Tools) != 3 {
				t.Fatalf("tools = %d", len(r.Tools))
			}
			if r.Tools[0].Strict == nil || !*r.Tools[0].Strict {
				t.Errorf("ping strict 应解出 true：%+v", r.Tools[0].Strict)
			}
			if r.Tools[1].Strict == nil || *r.Tools[1].Strict {
				t.Errorf("pong 显式 false 应保真（非 nil）：%+v", r.Tools[1].Strict)
			}
			if r.Tools[2].Strict != nil {
				t.Errorf("plain 没给应为 nil：%+v", r.Tools[2].Strict)
			}
		})
	}
}

// 同族回写：true/false 都回写，nil 不发明键。codex 同形。
func TestToolStrictRoundTrip(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 100,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{
			{Name: "ping", InputSchema: []byte(`{"type":"object"}`), Strict: boolPtr(true)},
			{Name: "pong", InputSchema: []byte(`{"type":"object"}`), Strict: boolPtr(false)},
			{Name: "plain", InputSchema: []byte(`{"type":"object"}`)},
		}}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "codex"} {
		t.Run(name, func(t *testing.T) {
			out, err := proto.MustOutbound(name).EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := string(out)
			if !strings.Contains(body, `"strict":true`) || !strings.Contains(body, `"strict":false`) {
				t.Errorf("strict 三态回写缺失: %s", body)
			}
			if strings.Count(body, `"strict"`) != 2 {
				t.Errorf("nil 不应发明 strict 键: %s", body)
			}
		})
	}
}

// R62：anthropic 工具修饰四维跨族零泄漏。
func TestToolModifiersNeverLeakToOtherFamilies(t *testing.T) {
	fa := false
	req := &ir.Request{Model: "m", MaxTokens: 100,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools: []ir.Tool{{Name: "ping", InputSchema: []byte(`{"type":"object"}`),
			DeferLoading: true, EagerInputStreaming: &fa,
			InputExamples:  []json.RawMessage{[]byte(`{"name":"x"}`)},
			AllowedCallers: []string{"direct"}}}}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		out, err := proto.MustOutbound(name).EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", name, err)
		}
		body := string(out)
		for _, probe := range []string{"defer_loading", "eager_input_streaming", "input_examples", "allowed_callers"} {
			if strings.Contains(body, probe) {
				t.Errorf("%s 泄漏 %q: %s", name, probe, body)
			}
		}
	}
}
