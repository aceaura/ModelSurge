package openaichat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R68：chat 请求侧 modalities/audio/prediction/web_search_options 四维贯通。
// 官方 SDK 核对（openai-sdk-typescript completions.ts:2314/2408/2436/2662）：
// 四维都是 chat 一族专属（responses 全系零命中）。modalities 简值数组；
// audio 的 voice 两形态（内置名 string / 自定义 {id} 对象）归一成 string；
// prediction 与 web_search_options 嵌套对象原文透传。

const chatReqPrefix = `{"model":"m","messages":[{"role":"user","content":"hi"}],`

func TestChatExtrasDecode(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(chatReqPrefix +
		`"modalities":["text","audio"],` +
		`"audio":{"format":"mp3","voice":"alloy"},` +
		`"prediction":{"type":"content","content":"const x = 1"},` +
		`"web_search_options":{"search_context_size":"high"}}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.Modalities) != 2 || r.Modalities[1] != "audio" {
		t.Errorf("Modalities = %v", r.Modalities)
	}
	if r.AudioOut == nil || r.AudioOut.Format != "mp3" || r.AudioOut.Voice != "alloy" {
		t.Errorf("AudioOut = %+v", r.AudioOut)
	}
	if string(r.Prediction) != `{"type":"content","content":"const x = 1"}` {
		t.Errorf("Prediction = %s", r.Prediction)
	}
	if string(r.WebSearchOptions) != `{"search_context_size":"high"}` {
		t.Errorf("WebSearchOptions = %s", r.WebSearchOptions)
	}
}

// voice 的 {id} 对象形态归一成 string（语义等价，回写取最简）。
func TestChatExtrasVoiceObjectForm(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(chatReqPrefix +
		`"modalities":["audio"],"audio":{"format":"wav","voice":{"id":"voice_1234"}}}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if r.AudioOut == nil || r.AudioOut.Voice != "voice_1234" {
		t.Fatalf("voice 对象形态归一失败：%+v", r.AudioOut)
	}
	out, err := codec{}.EncodeRequest(r)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"voice":"voice_1234"`) {
		t.Errorf("回写应取 string 简形：%s", out)
	}
}

func TestChatExtrasDecodeAbsentAndNull(t *testing.T) {
	r, err := codec{}.DecodeRequest([]byte(chatReqPrefix +
		`"prediction":null,"web_search_options":null}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(r.Modalities) != 0 || r.AudioOut != nil ||
		len(r.Prediction) != 0 || len(r.WebSearchOptions) != 0 {
		t.Errorf("null/缺席应全空：%+v", r)
	}
}

func TestChatExtrasEncodeRoundTrip(t *testing.T) {
	req := &ir.Request{Model: "m",
		Modalities:       []string{"text", "audio"},
		AudioOut:         &ir.AudioOutParam{Format: "opus", Voice: "cedar"},
		Prediction:       []byte(`{"type":"content","content":"abc"}`),
		WebSearchOptions: []byte(`{"user_location":{"type":"approximate"}}`),
		Messages:         []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"modalities":["text","audio"]`,
		`"audio":{"format":"opus","voice":"cedar"}`,
		`"prediction":{"type":"content","content":"abc"}`,
		`"web_search_options":{"user_location":{"type":"approximate"}}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("缺 %s：%s", want, s)
		}
	}
	// 同族往返不漂移。
	back, err := codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(往返): %v", err)
	}
	if len(back.Modalities) != 2 || back.AudioOut == nil || back.AudioOut.Voice != "cedar" ||
		string(back.Prediction) == "" || string(back.WebSearchOptions) == "" {
		t.Errorf("往返漂移：%+v", back)
	}
	// 零值不出键。
	out2, _ := codec{}.EncodeRequest(&ir.Request{Model: "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}})
	s2 := string(out2)
	for _, key := range []string{"modalities", "audio", "prediction", "web_search_options"} {
		if strings.Contains(s2, `"`+key+`"`) {
			t.Errorf("零值不应出键 %s：%s", key, s2)
		}
	}
}

// 字节级断言辅助：确认 prediction 原文未被子串替换破坏。
func TestChatExtrasPredictionByteExact(t *testing.T) {
	raw := `{"type":"content","content":[{"type":"text","text":"a \"quoted\" 中文"}]}`
	req := &ir.Request{Model: "m", Prediction: []byte(raw),
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(wire["prediction"]) != raw {
		t.Errorf("prediction 非字节保真：%s", wire["prediction"])
	}
}
