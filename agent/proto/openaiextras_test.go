package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R59 OpenAI 两系 2026 字段贯通：verbosity（chat 顶层 / responses text.verbosity）、
// safety_identifier（user 的官方替代）、moderation、prompt_cache_options。
// safety_identifier 与 user 同一维度（滥用检测标识），跨族到 anthropic 映进
// metadata.user_id；moderation 与 prompt_cache_options 结构属上游产品语义，
// 不透明原文透传。
// 参考仓对照：cc-switch/new-api 对这四项全部静默丢。

const r59SafetyClue = "sid-r59-clue-3f9"

func mkExtrasReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 100,
		Verbosity:          "low",
		SafetyIdentifier:   r59SafetyClue,
		Moderation:         json.RawMessage(`{"model":"omni-moderation-latest","policy":{"input":{"mode":"block"}}}`),
		PromptCacheOptions: json.RawMessage(`{"mode":"explicit","ttl":"30m"}`),
		Messages:           []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
}

// 解码：两系各自把四项收进 IR；verbosity 在 responses 里从 text.verbosity 取。
func TestOpenAIExtrasDecodeIntoIR(t *testing.T) {
	mod := `{"model":"omni-moderation-latest"}`
	pco := `{"ttl":"30m"}`
	for _, c := range []struct{ name, body, wantVerbosity string }{
		{"openai-chat", `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"verbosity":"high","safety_identifier":"` + r59SafetyClue + `",` +
			`"moderation":` + mod + `,"prompt_cache_options":` + pco + `}`, "high"},
		{"openai-responses", `{"model":"m","input":"hi",` +
			`"text":{"verbosity":"low"},"safety_identifier":"` + r59SafetyClue + `",` +
			`"moderation":` + mod + `,"prompt_cache_options":` + pco + `}`, "low"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := proto.MustInbound(c.name).DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if r.Verbosity != c.wantVerbosity {
				t.Errorf("Verbosity = %q, want %q", r.Verbosity, c.wantVerbosity)
			}
			if r.SafetyIdentifier != r59SafetyClue {
				t.Errorf("SafetyIdentifier = %q", r.SafetyIdentifier)
			}
			if string(r.Moderation) != mod {
				t.Errorf("Moderation = %s", r.Moderation)
			}
			if string(r.PromptCacheOptions) != pco {
				t.Errorf("PromptCacheOptions = %s", r.PromptCacheOptions)
			}
		})
	}
}

// 同族回写：verbosity 在 chat 是顶层、在 responses 必须落在 text 容器里
// （哪怕没有 format 要求）；其余三项顶层原样回家。codex 同形。
func TestOpenAIExtrasRoundTripSameFamily(t *testing.T) {
	req := mkExtrasReq()
	for _, c := range []struct{ name, verbosityProbe string }{
		{"openai-chat", `"verbosity":"low"`},
		{"openai-responses", `"text":{"verbosity":"low"}`},
		{"codex", `"text":{"verbosity":"low"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := proto.MustOutbound(c.name).EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := string(out)
			if !strings.Contains(body, c.verbosityProbe) {
				t.Errorf("verbosity 落点不对: %s", body)
			}
			for _, probe := range []string{`"safety_identifier":"` + r59SafetyClue + `"`,
				`"moderation":{"model":"omni-moderation-latest"`,
				`"prompt_cache_options":{"mode":"explicit"`} {
				if !strings.Contains(body, probe) {
					t.Errorf("缺 %q: %s", probe, body)
				}
			}
		})
	}
}

// verbosity 与 format 并存时同容器不互相覆盖。
func TestVerbosityCoexistsWithFormat(t *testing.T) {
	req := mkExtrasReq()
	req.ResponseFormat = &ir.ResponseFormat{Schema: json.RawMessage(`{"type":"object"}`), Strict: true}
	out, err := proto.MustOutbound("openai-responses").EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	body := string(out)
	if !strings.Contains(body, `"format":{"type":"json_schema"`) || !strings.Contains(body, `"verbosity":"low"`) {
		t.Errorf("text 容器应同时承载 format 与 verbosity: %s", body)
	}
}

// safety_identifier 跨族到 anthropic：user_id 槽空着时映进 metadata.user_id；
// 被占时 user 优先，safety identifier 丢弃（由诊断报出）。
func TestSafetyIdentifierMapsToAnthropicMetadata(t *testing.T) {
	req := mkExtrasReq()
	out, err := proto.MustOutbound("anthropic").EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"user_id":"`+r59SafetyClue+`"`) {
		t.Errorf("safety identifier 应映进 metadata.user_id: %s", out)
	}
	// 两边都有：user 优先
	req.Metadata = map[string]string{"user_id": "real-user-1"}
	out, err = proto.MustOutbound("anthropic").EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	body := string(out)
	if !strings.Contains(body, `"user_id":"real-user-1"`) {
		t.Errorf("user_id 应优先: %s", body)
	}
	if strings.Contains(body, r59SafetyClue) {
		t.Errorf("被挤掉的 safety identifier 不得泄漏: %s", body)
	}
}

// 四项对 anthropic（safety_identifier 除外）与 kiro 一个字符都不进载荷。
func TestOpenAIExtrasNeverLeakToOtherFamilies(t *testing.T) {
	req := mkExtrasReq()
	for _, name := range []string{"anthropic", "kiro"} {
		out, err := proto.MustOutbound(name).EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", name, err)
		}
		body := string(out)
		for _, probe := range []string{"verbosity", "moderation", "prompt_cache_options",
			"omni-moderation-latest", "safety_identifier"} {
			if strings.Contains(body, probe) {
				t.Errorf("%s 泄漏 %q: %s", name, probe, body)
			}
		}
		if name == "kiro" && strings.Contains(body, r59SafetyClue) {
			t.Errorf("kiro 泄漏 safety identifier 值: %s", body)
		}
	}
}

// 显式 null 等同没给：Clone 往返后出站也不得出现 null 键。
func TestExplicitNullModerationTreatedAsAbsent(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"openai-chat", `{"model":"m","messages":[{"role":"user","content":"hi"}],"moderation":null,"prompt_cache_options":null}`},
		{"openai-responses", `{"model":"m","input":"hi","moderation":null,"prompt_cache_options":null}`},
	} {
		r, err := proto.MustInbound(c.name).DecodeRequest([]byte(c.body))
		if err != nil {
			t.Fatalf("%s DecodeRequest: %v", c.name, err)
		}
		if len(r.Moderation) > 0 || len(r.PromptCacheOptions) > 0 {
			t.Errorf("%s 显式 null 应归一为缺省：%+v", c.name, r)
		}
		out, err := proto.MustOutbound(c.name).EncodeRequest(r)
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", c.name, err)
		}
		if strings.Contains(string(out), "moderation") || strings.Contains(string(out), "prompt_cache_options") {
			t.Errorf("%s null 键泄漏进出站载荷: %s", c.name, out)
		}
	}
}

// 全缺省：IR 零值，出站不多一个键。
func TestOpenAIExtrasAbsentStayZero(t *testing.T) {
	r, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.Verbosity != "" || r.SafetyIdentifier != "" || len(r.Moderation) > 0 || len(r.PromptCacheOptions) > 0 {
		t.Errorf("缺省请求解出了东西：%+v", r)
	}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		out, _ := proto.MustOutbound(name).EncodeRequest(&ir.Request{Model: "m", MaxTokens: 100,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}})
		body := string(out)
		for _, probe := range []string{`"verbosity"`, `"safety_identifier"`, `"moderation"`, `"prompt_cache_options"`} {
			if strings.Contains(body, probe) {
				t.Errorf("%s 缺省时发明键 %q: %s", name, probe, body)
			}
		}
	}
}
