package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// structuredoutput_test.go 结构化输出（JSON 模式 / JSON Schema）跨协议投影。
//
// 三种入站形态：Chat 的 response_format、Responses 的 text.format、
// Gemini 的 responseMimeType + responseSchema。Anthropic 与 kiro 没有落点，
// 由 Capabilities.StructuredOutput + 诊断兜底（见 relay 侧测试）。

const schemaLiteral = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

// ---- 入站解码 ----

func TestChatDecodesJSONSchema(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"weather","strict":true,"schema":` + schemaLiteral + `}}}`)
	req, err := proto.MustInbound("openai-chat").DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	assertSchemaFormat(t, req.ResponseFormat, "weather", true)
}

func TestChatDecodesJSONObjectMode(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	req, err := proto.MustInbound("openai-chat").DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.ResponseFormat == nil {
		t.Fatal("json_object 未进 IR")
	}
	// 无 schema 的 JSON 模式与带 schema 的约束是两档语义，不能混同。
	if req.ResponseFormat.IsSchema() {
		t.Errorf("json_object 不该带 schema：%q", req.ResponseFormat.Schema)
	}
}

// strict 缺省与 strict:false 都必须解成非严格。这一格此前从未被构造过：
// 所有 schema 夹具都带 strict:true，于是「解码恒置 true」与正确实现无法区分，
// 而恒 true 会把客户端的宽松约束升级成严格，上游会因 schema 不完备而 400。
func TestDecodeNonStrictSchema(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"openai-chat/absent", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"weather","schema":` + schemaLiteral + `}}}`},
		{"openai-chat/false", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"weather","strict":false,"schema":` + schemaLiteral + `}}}`},
		{"openai-responses/absent", `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"weather","schema":` + schemaLiteral + `}}}`},
		{"openai-responses/false", `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"weather","strict":false,"schema":` + schemaLiteral + `}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			name := strings.SplitN(c.name, "/", 2)[0]
			req, err := proto.MustInbound(name).DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			assertSchemaFormat(t, req.ResponseFormat, "weather", false)
		})
	}
}

func TestResponsesDecodesTextFormat(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"weather","strict":true,"schema":` + schemaLiteral + `}}}`)
	req, err := proto.MustInbound("openai-responses").DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	assertSchemaFormat(t, req.ResponseFormat, "weather", true)
}

func TestGeminiDecodesResponseSchema(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":` + schemaLiteral + `}}`)
	req, err := proto.MustInbound("gemini").DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	// Gemini 的 responseSchema 恒为严格语义，没有 strict 开关。
	assertSchemaFormat(t, req.ResponseFormat, "", true)
}

// 只写 mimeType 不给 schema 也是结构化输出诉求（JSON 模式那一档）。
func TestGeminiDecodesJSONMimeWithoutSchema(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json"}}`)
	req, err := proto.MustInbound("gemini").DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.ResponseFormat == nil {
		t.Fatal("responseMimeType=application/json 未进 IR")
	}
	if req.ResponseFormat.IsSchema() {
		t.Errorf("没给 schema 却凭空造了一个：%q", req.ResponseFormat.Schema)
	}
}

// 反过来：只给 schema 不写 mimeType 同样不能漏。
func TestGeminiDecodesSchemaWithoutMime(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseSchema":` + schemaLiteral + `}}`)
	req, err := proto.MustInbound("gemini").DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if !req.ResponseFormat.IsSchema() {
		t.Fatalf("只给 responseSchema 时整条漏掉：%+v", req.ResponseFormat)
	}
}

// type:"text" 是协议默认值，等同于「客户端没提要求」，不能进 IR——
// 否则下游会为一个不存在的诉求触发诊断，并给 anthropic/kiro 报假有损。
func TestPlainTextFormatDoesNotEnterIR(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"openai-chat", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`},
		{"openai-responses", `{"model":"m","input":"hi","text":{"format":{"type":"text"}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := proto.MustInbound(c.name).DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if req.ResponseFormat != nil {
				t.Errorf("type:text 不该进 IR：%+v", req.ResponseFormat)
			}
		})
	}
}

// 没写结构化输出字段时 IR 必须留空。
func TestAbsentFormatLeavesIRNil(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"openai-chat", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"openai-responses", `{"model":"m","input":"hi"}`},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5}}`},
		{"anthropic", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := proto.MustInbound(c.name).DecodeRequest([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if req.ResponseFormat != nil {
				t.Errorf("无字段却造出约束：%+v", req.ResponseFormat)
			}
		})
	}
}

// ---- 出站编码 ----

func TestChatEncodesJSONSchema(t *testing.T) {
	body := encodeReq(t, "openai-chat", schemaReq(true))
	var got struct {
		ResponseFormat *struct {
			Type       string `json:"type"`
			JSONSchema *struct {
				Name   string          `json:"name"`
				Strict *bool           `json:"strict"`
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if got.ResponseFormat == nil || got.ResponseFormat.JSONSchema == nil {
		t.Fatalf("response_format 没写出去：%s", body)
	}
	if got.ResponseFormat.Type != "json_schema" {
		t.Errorf("type = %q, want json_schema", got.ResponseFormat.Type)
	}
	if got.ResponseFormat.JSONSchema.Name != "weather" {
		t.Errorf("name = %q, want weather", got.ResponseFormat.JSONSchema.Name)
	}
	if got.ResponseFormat.JSONSchema.Strict == nil || !*got.ResponseFormat.JSONSchema.Strict {
		t.Errorf("strict 丢了：%s", body)
	}
	assertSameSchema(t, got.ResponseFormat.JSONSchema.Schema)
}

func TestResponsesEncodesTextFormat(t *testing.T) {
	body := encodeReq(t, "openai-responses", schemaReq(true))
	var got struct {
		Text *struct {
			Format *struct {
				Type   string          `json:"type"`
				Name   string          `json:"name"`
				Strict *bool           `json:"strict"`
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		} `json:"text"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if got.Text == nil || got.Text.Format == nil {
		t.Fatalf("text.format 没写出去：%s", body)
	}
	if got.Text.Format.Type != "json_schema" {
		t.Errorf("type = %q, want json_schema", got.Text.Format.Type)
	}
	if got.Text.Format.Name != "weather" {
		t.Errorf("name = %q, want weather", got.Text.Format.Name)
	}
	if got.Text.Format.Strict == nil || !*got.Text.Format.Strict {
		t.Errorf("strict 丢了：%s", body)
	}
	assertSameSchema(t, got.Text.Format.Schema)
}

// 无 schema 的 JSON 模式退回 json_object：这一档语义 json_object 能完整表达，
// 不能升格成 json_schema（那要求一个并不存在的 schema）。
func TestEncodeJSONModeWithoutSchema(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 100, Messages: []ir.Message{userMsg("hi")}, ResponseFormat: &ir.ResponseFormat{}}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			body := string(encodeReq(t, name, req))
			if !strings.Contains(body, `"type":"json_object"`) {
				t.Errorf("无 schema 时未退回 json_object：%s", body)
			}
			if strings.Contains(body, "json_schema") {
				t.Errorf("凭空升格成 json_schema：%s", body)
			}
		})
	}
}

// strict 未开时不能写出 strict:true——它会让上游对 schema 做额外校验并可能 400。
func TestEncodeOmitsStrictWhenUnset(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			body := string(encodeReq(t, name, schemaReq(false)))
			if strings.Contains(body, "strict") {
				t.Errorf("未要求严格却写出 strict：%s", body)
			}
		})
	}
}

// name 是 json_schema 的必填字段，跨协议来的请求（如 Gemini 入站）没有名称，
// 不补默认值上游会 400。
func TestEncodeFillsSchemaName(t *testing.T) {
	req := schemaReq(true)
	req.ResponseFormat.Name = ""
	for _, name := range []string{"openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			body := string(encodeReq(t, name, req))
			if !strings.Contains(body, `"name":"response"`) {
				t.Errorf("缺 name 时未补默认值：%s", body)
			}
		})
	}
}

// 没有约束时不能凭空冒出 response_format / text 壳。
func TestEncodeOmitsFormatWhenAbsent(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 100, Messages: []ir.Message{userMsg("hi")}}
	for name, key := range map[string]string{
		"openai-chat": "response_format",
		// 不能只查 "text"：正文块里就有 "text":"hi"。查容器整体。
		"openai-responses": `"text":{`,
	} {
		t.Run(name, func(t *testing.T) {
			if body := string(encodeReq(t, name, req)); strings.Contains(body, key) {
				t.Errorf("无约束却写出 %s：%s", key, body)
			}
		})
	}
}

// ---- 跨协议往返 ----

// Gemini -> OpenAI 两系：Gemini 没有名称，schema 本体必须原样过去。
func TestGeminiSchemaSurvivesToOpenAI(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":` + schemaLiteral + `}}`)
	req, err := proto.MustInbound("gemini").DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	req.MaxTokens = 100
	for _, name := range []string{"openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			out := encodeReq(t, name, req)
			if !strings.Contains(string(out), `"city"`) {
				t.Errorf("schema 本体没过去：%s", out)
			}
			back, err := proto.MustInbound(name).DecodeRequest(out)
			if err != nil {
				t.Fatalf("回解: %v", err)
			}
			if !back.ResponseFormat.IsSchema() {
				t.Fatalf("往返后约束丢了：%+v", back.ResponseFormat)
			}
			assertSameSchema(t, back.ResponseFormat.Schema)
		})
	}
}

// 无落点的上游（anthropic / kiro）必须声明能力缺失，由诊断层报出。
func TestProtocolsWithoutStructuredOutputDeclareIt(t *testing.T) {
	for name, want := range map[string]bool{
		"anthropic": false, "kiro": false,
		"openai-chat": true, "openai-responses": true,
	} {
		t.Run(name, func(t *testing.T) {
			if got := proto.MustOutbound(name).Caps().StructuredOutput; got != want {
				t.Errorf("StructuredOutput = %v, want %v", got, want)
			}
		})
	}
}

// ---- 夹具 ----

func schemaReq(strict bool) *ir.Request {
	return &ir.Request{
		Model:     "m",
		MaxTokens: 100,
		Messages:  []ir.Message{userMsg("hi")},
		ResponseFormat: &ir.ResponseFormat{
			Name:   "weather",
			Schema: json.RawMessage(schemaLiteral),
			Strict: strict,
		},
	}
}

func userMsg(text string) ir.Message {
	return ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: text}}}
}

func encodeReq(t *testing.T, name string, req *ir.Request) []byte {
	t.Helper()
	body, err := proto.MustOutbound(name).EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest(%s): %v", name, err)
	}
	return body
}

func assertSchemaFormat(t *testing.T, f *ir.ResponseFormat, wantName string, wantStrict bool) {
	t.Helper()
	if f == nil {
		t.Fatal("结构化输出约束未进 IR")
	}
	if f.Name != wantName {
		t.Errorf("Name = %q, want %q", f.Name, wantName)
	}
	if f.Strict != wantStrict {
		t.Errorf("Strict = %v, want %v", f.Strict, wantStrict)
	}
	if !f.IsSchema() {
		t.Fatal("schema 本体丢了")
	}
	assertSameSchema(t, f.Schema)
}

// assertSameSchema 比较 schema 语义而非字节：各 codec 经 json.Marshal
// 重排后键序可能不同，逐字节比会把等价结果判成失败。
func assertSameSchema(t *testing.T, got json.RawMessage) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("schema 不是合法 JSON：%s", got)
	}
	if err := json.Unmarshal([]byte(schemaLiteral), &b); err != nil {
		t.Fatalf("夹具坏了：%v", err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("schema 被改写了：\n got %s\nwant %s", ja, jb)
	}
}
