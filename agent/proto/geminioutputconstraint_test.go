package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R100 扩点：gemini 的输出约束两维。探针日志 r100_probe5.log 实测：
//
//   - generationConfig.responseModalities 整条不进 IR（结构体里根本没这个字段，
//     json.Unmarshal 静默忽略）。客户端写明「要 TEXT+IMAGE 输出」，转去 chat
//     上游时连「要非文本输出」这件事都传达不到，响应必是纯文本，且任何注记
//     都没有——既有的 modalities 注记门控在 len(Modalities)>0 上，永远空。
//   - 非 JSON 的 responseMimeType（text/x.enum 等）同样静默丢：解码只认
//     application/json，别的值连「客户端约束过输出类型」的痕迹都不留。
//
// 修法：responseModalities 归一成 OpenAI 风格小写值进 IR.Modalities（text/
// audio 跨族可达 chat，image 由 chat 出站过滤 + 诊断报出）；非 JSON 且非
// text/plain 的 responseMimeType 进 IR.ResponseMimeType，四个出站都接不住，
// 由诊断恒报。

func r100dGemini(t *testing.T, generationConfig string) *ir.Request {
	t.Helper()
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":` + generationConfig + `}`
	req, err := proto.MustInbound("gemini").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("gemini DecodeRequest: %v", err)
	}
	return req
}

// responseModalities 归一小写进 IR；缺省保持 nil。
func TestGeminiResponseModalitiesReachIR(t *testing.T) {
	req := r100dGemini(t, `{"responseModalities":["TEXT","IMAGE"]}`)
	if strings.Join(req.Modalities, ",") != "text,image" {
		t.Errorf("Modalities = %v, want [text image]", req.Modalities)
	}
	mixed := r100dGemini(t, `{"responseModalities":["Audio"]}`)
	if strings.Join(mixed.Modalities, ",") != "audio" {
		t.Errorf("大小写混合没归一：%v", mixed.Modalities)
	}
	absent := r100dGemini(t, `{"maxOutputTokens":64}`)
	if absent.Modalities != nil {
		t.Errorf("没给却非零：%v", absent.Modalities)
	}
}

// text/audio 跨族可达 chat 的 modalities：gemini 客户端要的音频输出真的能传到。
func TestGeminiAudioModalityReachesChatOutbound(t *testing.T) {
	req := r100dGemini(t, `{"responseModalities":["TEXT","AUDIO"]}`)
	out := r100bEnc(t, "openai-chat", req)
	if !strings.Contains(out, `"modalities":["text","audio"]`) {
		t.Errorf("chat 出站没带 modalities：%s", out)
	}
}

// image 是 chat 值集之外的模态：写出去是上游必 400 的形状，必须滤掉（报损见
// relay 侧诊断测试）。其余三族本就没有 modalities 槽位。
func TestGeminiImageModalityFilteredEverywhere(t *testing.T) {
	req := r100dGemini(t, `{"responseModalities":["TEXT","IMAGE"]}`)
	if got := r100bEnc(t, "openai-chat", req); strings.Contains(got, "image") {
		t.Errorf("chat 出站把 image 模态写上了线：%s", got)
	} else if !strings.Contains(got, `"modalities":["text"]`) {
		t.Errorf("chat 出站把 text 模态也吞了：%s", got)
	}
	for _, outbound := range []string{"anthropic", "codex", "openai-responses"} {
		if got := r100bEnc(t, outbound, req); strings.Contains(got, "modalities") {
			t.Errorf("%s 出站不该有 modalities 键：%s", outbound, got)
		}
	}
}

// chat 自家请求的 modalities 不受过滤器影响：text/audio 原样过。
func TestChatModalitiesUnaffectedByFilter(t *testing.T) {
	req, err := proto.MustInbound("openai-chat").DecodeRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"modalities":["text","audio"],"audio":{"voice":"alloy","format":"wav"}}`))
	if err != nil {
		t.Fatalf("chat DecodeRequest: %v", err)
	}
	out := r100bEnc(t, "openai-chat", req)
	if !strings.Contains(out, `"modalities":["text","audio"]`) {
		t.Errorf("chat 同族往返丢了 modalities：%s", out)
	}
}

// 非 JSON 的 responseMimeType 进 IR 原样保留；text/plain 是显式缺省不收；
// application/json 与带 schema 的情形照旧走 ResponseFormat。
func TestGeminiNonJSONMimeReachesIR(t *testing.T) {
	req := r100dGemini(t, `{"responseMimeType":"text/x.enum"}`)
	if req.ResponseMimeType != "text/x.enum" {
		t.Errorf("ResponseMimeType = %q, want text/x.enum", req.ResponseMimeType)
	}
	if req.ResponseFormat != nil {
		t.Errorf("非 JSON MIME 不该造出 ResponseFormat：%+v", req.ResponseFormat)
	}

	plain := r100dGemini(t, `{"responseMimeType":"text/plain"}`)
	if plain.ResponseMimeType != "" || plain.ResponseFormat != nil {
		t.Errorf("text/plain 是显式缺省，不该留痕：mime=%q format=%+v", plain.ResponseMimeType, plain.ResponseFormat)
	}

	js := r100dGemini(t, `{"responseMimeType":"application/json"}`)
	if js.ResponseFormat == nil || js.ResponseMimeType != "" {
		t.Errorf("application/json 该走 ResponseFormat：mime=%q format=%+v", js.ResponseMimeType, js.ResponseFormat)
	}

	schema := r100dGemini(t, `{"responseSchema":{"type":"OBJECT"}}`)
	if schema.ResponseFormat == nil || schema.ResponseMimeType != "" {
		t.Errorf("裸 schema 该走 ResponseFormat：mime=%q format=%+v", schema.ResponseMimeType, schema.ResponseFormat)
	}
}
