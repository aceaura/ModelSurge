package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/kiro"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// user 维度横切：anthropic 的 metadata.user_id 与 chat/responses 的 user 是
// 同一维度，IR 统一放 Metadata["user_id"]。三路入站都要解出来；出站只有
// kiro 没有这一维，不得泄漏也不得编造。

const userClue = "u-r53-clue-9x7"

func userInboundBodies() map[string]string {
	return map[string]string{
		"anthropic":        `{"model":"m","max_tokens":8,"metadata":{"user_id":"` + userClue + `"},"messages":[{"role":"user","content":"hi"}]}`,
		"openai-chat":      `{"model":"m","user":"` + userClue + `","messages":[{"role":"user","content":"hi"}]}`,
		"openai-responses": `{"model":"m","user":"` + userClue + `","input":"hi"}`,
	}
}

func TestUserIDDecodesIntoIR(t *testing.T) {
	for name, body := range userInboundBodies() {
		r, err := proto.MustInbound(name).DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: DecodeRequest err=%v", name, err)
		}
		if got := r.Metadata["user_id"]; got != userClue {
			t.Errorf("%s: Metadata[user_id]=%q, want %q", name, got, userClue)
		}
	}
}

// 三个入站解出的 user 必须能送达有这一维的全部出站。
func TestUserIDReachesCapableUpstreams(t *testing.T) {
	for inName, body := range userInboundBodies() {
		r, err := proto.MustInbound(inName).DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: DecodeRequest err=%v", inName, err)
		}
		for _, outName := range []string{"anthropic", "openai-chat", "openai-responses", "codex"} {
			out, err := proto.MustOutbound(outName).EncodeRequest(r)
			if err != nil {
				t.Errorf("%s->%s: EncodeRequest err=%v", inName, outName, err)
				continue
			}
			if !strings.Contains(string(out), userClue) {
				t.Errorf("%s->%s: user id 没到出站载荷: %s", inName, outName, out)
			}
		}
	}
}

// kiro 没有 user 槽位：线索值一个字符都不许出现在载荷里（kiro 载荷带随机
// hex 会话 id，按值探会假阳性，这里探的是客户端给的线索串本身）。
func TestUserIDNeverLeaksIntoKiro(t *testing.T) {
	for inName, body := range userInboundBodies() {
		r, _ := proto.MustInbound(inName).DecodeRequest([]byte(body))
		out, err := proto.MustOutbound("kiro").EncodeRequest(r)
		if err != nil {
			t.Fatalf("%s->kiro: EncodeRequest err=%v", inName, err)
		}
		if strings.Contains(string(out), userClue) {
			t.Errorf("%s->kiro: user id 泄漏进无此维度的协议: %s", inName, out)
		}
	}
}

// 客户端没给 user 时，任何出站都不得出现该键——零值表缺省是替客户端表态。
func TestUserIDNotInventedWhenAbsent(t *testing.T) {
	bodies := map[string]string{
		"anthropic":        `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		"openai-chat":      `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		"openai-responses": `{"model":"m","input":"hi"}`,
	}
	for inName, body := range bodies {
		r, err := proto.MustInbound(inName).DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: DecodeRequest err=%v", inName, err)
		}
		if len(r.Metadata) != 0 {
			t.Errorf("%s: 没给 user 却造出 Metadata: %v", inName, r.Metadata)
		}
		for _, outName := range proto.OutboundNames() {
			out, err := proto.MustOutbound(outName).EncodeRequest(r)
			if err != nil {
				t.Errorf("%s->%s: EncodeRequest err=%v", inName, outName, err)
				continue
			}
			s := string(out)
			// 探键值形态而非裸键名：消息里的 "role":"user" 含裸键子串。
			if strings.Contains(s, `"user":"`) || strings.Contains(s, `"user_id":"`) {
				t.Errorf("%s->%s: 没给 user 却造出该键: %s", inName, outName, s)
			}
		}
	}
}

// Gemini 专属维度入 IR：safetySettings 与 cachedContent 只有这一族有，
// 解不出来 Diagnose 就永远看不见「客户端给了但装不下」。
func TestGeminiOnlyFieldsDecodeIntoIR(t *testing.T) {
	body := `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"safetySettings":[{"category":"HARM_CATEGORY_HATE_SPEECH","threshold":"BLOCK_ONLY_HIGH"},
			{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","threshold":"OFF"}],
		"cachedContent":"cachedContents/abc123"}`
	r, err := proto.MustInbound("gemini").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if len(r.SafetySettings) != 2 {
		t.Fatalf("SafetySettings=%v, want 2 条", r.SafetySettings)
	}
	if r.SafetySettings[0].Category != "HARM_CATEGORY_HATE_SPEECH" || r.SafetySettings[0].Threshold != "BLOCK_ONLY_HIGH" {
		t.Errorf("SafetySettings[0]=%+v", r.SafetySettings[0])
	}
	if r.SafetySettings[1].Threshold != "OFF" {
		t.Errorf("SafetySettings[1]=%+v", r.SafetySettings[1])
	}
	if r.CachedContent != "cachedContents/abc123" {
		t.Errorf("CachedContent=%q", r.CachedContent)
	}
}

// 没给这两维时 IR 保持零值，诊断不得误报。
func TestGeminiOnlyFieldsAbsentStayZero(t *testing.T) {
	body := `{"model":"m","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	r, err := proto.MustInbound("gemini").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if len(r.SafetySettings) != 0 || r.CachedContent != "" {
		t.Errorf("没给却非零：%+v %q", r.SafetySettings, r.CachedContent)
	}
}
