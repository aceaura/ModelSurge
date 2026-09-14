package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// 请求方向：仅本族形态签名可还原 reasoning item（encrypted_content）；
// 外族/来源不明的签名跳过，避免构造上游无法解密的 item。
func TestEncodeRequest_ReasoningItem(t *testing.T) {
	mk := func(sig, from string) *ir.Request {
		return &ir.Request{
			Model: "gpt-x",
			Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
				{Role: ir.RoleAssistant, Content: []ir.Block{
					{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm", Signature: sig, SignatureFrom: from}},
					{Type: ir.BlockText, Text: "ok"},
				}},
				{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
			},
		}
	}

	out, err := New().EncodeRequest(mk("enc", "openai-responses"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"encrypted_content"`) {
		t.Errorf("same-protocol signature should round-trip as reasoning item: %s", out)
	}

	for _, from := range []string{"anthropic", ""} {
		out, err := New().EncodeRequest(mk("enc", from))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), `"encrypted_content"`) {
			t.Errorf("foreign signature (from=%q) must not become a reasoning item: %s", from, out)
		}
	}
}

// DecodeRequest 官方 Responses input 形态全兼容：
// 纯字符串、省略 type 的 message item（role 推断）、带 type 的标准形态。
func TestDecodeRequest_InputShapes(t *testing.T) {
	mustUser := func(t *testing.T, body string) *ir.Request {
		t.Helper()
		req, err := New().DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser {
			t.Fatalf("messages = %+v, want single user message", req.Messages)
		}
		if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Text != "hi" {
			t.Fatalf("content = %+v, want text hi", req.Messages[0].Content)
		}
		return req
	}

	// 纯字符串 input（最简形态）
	mustUser(t, `{"model":"m","input":"hi"}`)
	// 字符串 input + instructions 进 system
	req := mustUser(t, `{"model":"m","instructions":"be nice","input":"hi"}`)
	if len(req.System) != 1 {
		t.Fatalf("system = %+v, want instructions", req.System)
	}
	// 数组 item 省略 type（role 推断为 message）
	mustUser(t, `{"model":"m","input":[{"role":"user","content":"hi"}]}`)
	// 数组 item 带 type + content 纯字符串
	mustUser(t, `{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`)
	// 数组 item 带 type + content parts
	mustUser(t, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	// 无 role 无 type 的未知 item 不产生消息（也不报错）
	req, err := New().DecodeRequest([]byte(`{"model":"m","input":[{"id":"it_1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 0 {
		t.Fatalf("unknown items must be skipped, got %+v", req.Messages)
	}
}

// codex 别名 codec：协议名独立注册，编解码与 openai-responses 同一实现；
// instructions 恒存在（订阅端点要求字段，无 system 时输出空串）。
func TestCodexCodec(t *testing.T) {
	c := New()
	cx := codexCodec{codec: codec{}}
	if c.Name() != "openai-responses" || cx.Name() != NameCodex {
		t.Fatalf("codec names = %q / %q", c.Name(), cx.Name())
	}
	if _, err := proto.GetOutbound(NameCodex); err != nil {
		t.Fatalf("codex codec not registered: %v", err)
	}
	out, err := cx.EncodeRequest(&ir.Request{
		Model:    "gpt-5.6-sol",
		Stream:   true,
		Thinking: &ir.ThinkingConfig{Enabled: true, Effort: "xhigh"},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"instructions":`, `"store":false`, `"effort":"xhigh"`, `reasoning.encrypted_content`} {
		if !strings.Contains(s, want) {
			t.Fatalf("encoded missing %s: %s", want, s)
		}
	}
}
