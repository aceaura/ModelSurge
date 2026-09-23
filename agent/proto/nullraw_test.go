package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R98：ir.Request.Clone() 走 JSON 往返，而每个 EncodeRequest 的第一句就是 Clone。
// nil 的 json.RawMessage 会被 marshal 成字面 null，解回来是 4 字节的非空 "null"，
// 于是「客户端没发这个字段」在出站被读成「客户端发了个 null」。实测四族出站：
//
//   - tool_use 无 input：chat/responses/codex 写出 "arguments":"null"（工具参数被
//     改写，客户端把这个 tool_call 回传时上游按 JSON 解析直接失败）；anthropic 写出
//     "input":{"_modelsurge_raw_args":null}（凭空多一个键）；目标轮次没有工具定义、
//     调用被降级成文本时，提示词里出现 "[Tool: f (t1)]\nnull"；
//   - 工具定义无 schema：anthropic "input_schema":null、chat/responses/codex
//     "parameters":null；
//   - responses custom tool 无 format："format":null；server_tool_use 无 input：
//     "input":null；response_format 只给 type 不给 schema："schema":null；
//     prompt 引用不带 variables："variables":null。
//
// 修法只能在 IR 层：出站 DTO 早就都带 omitempty，但 omitempty 压不住一个非空的
// 4 字节 "null"。字段补上 json:",omitempty" 后 nil 在 Clone 里保持 nil，缺省就是
// 缺省；客户端显式发来的 null 仍然逐字节透传，由上游按自己的口径报错。
// ir/rawmessage_test.go 用反射把「可达图里每个 RawMessage 字段都必须带 omitempty」
// 钉死，新增字段漏标签当场红。

// r98Outbound 出站四族，硬编码（含 codex，它与 openai-responses 同形）。
var r98Outbound = []string{"anthropic", "codex", "openai-chat", "openai-responses"}

func r98Decode(t *testing.T, inbound, body string) *ir.Request {
	t.Helper()
	req, err := proto.MustInbound(inbound).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s DecodeRequest: %v", inbound, err)
	}
	return req
}

func r98EncodeAll(t *testing.T, req *ir.Request) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range r98Outbound {
		body, err := proto.MustOutbound(name).EncodeRequest(req)
		if err != nil {
			t.Fatalf("%s EncodeRequest: %v", name, err)
		}
		out[name] = string(body)
		t.Logf("%-16s %s", name, body)
	}
	return out
}

func r98AssertNoNull(t *testing.T, out map[string]string, keys ...string) {
	t.Helper()
	for _, name := range r98Outbound {
		for _, k := range keys {
			if strings.Contains(out[name], k) {
				t.Errorf("%s 出站出现伪造的 %s：%s", name, k, out[name])
			}
		}
	}
}

// ---- 工具调用没有 arguments ----

// 没有工具定义的轮次里，助手的历史 tool_call 会被降级成文本（绕开上游的工具配对
// 约束）。降级路径直接把 Input 拼进提示词，nil 曾经拼出字面 "null"。
func TestToolUseWithoutArgumentsNotRenderedAsNullText(t *testing.T) {
	req := r98Decode(t, "openai-chat", `{"model":"m","max_tokens":16,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"f"}}]},
		{"role":"tool","tool_call_id":"t1","content":"ok"}]}`)
	out := r98EncodeAll(t, req)
	r98AssertNoNull(t, out, "null")
	for _, name := range r98Outbound {
		if !strings.Contains(out[name], "[Tool: f (t1)]") {
			t.Errorf("%s 降级文本丢了工具名/ID：%s", name, out[name])
		}
		if !strings.Contains(out[name], "{}") {
			t.Errorf("%s 无参调用没有按规范形态 {} 落地：%s", name, out[name])
		}
	}
}

// 有工具定义时走原生工具调用槽位：参数必须是空对象，不能是字符串 "null"，也不能
// 被挪进 _modelsurge_raw_args（那个键位是给畸形参数用的，无参不是畸形）。
func TestToolUseWithoutArgumentsKeepsEmptyObjectSlot(t *testing.T) {
	req := r98Decode(t, "openai-chat", `{"model":"m","max_tokens":16,
		"tools":[{"type":"function","function":{"name":"f","description":"d"}}],
		"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"t1","type":"function","function":{"name":"f"}}]},
		{"role":"tool","tool_call_id":"t1","content":"ok"}]}`)
	out := r98EncodeAll(t, req)
	r98AssertNoNull(t, out, `"arguments":"null"`, `"input":null`, "_modelsurge_raw_args")
	if !strings.Contains(out["anthropic"], `"input":{}`) {
		t.Errorf("anthropic 工具参数不是空对象：%s", out["anthropic"])
	}
	for _, name := range []string{"codex", "openai-chat", "openai-responses"} {
		if !strings.Contains(out[name], `"arguments":"{}"`) {
			t.Errorf("%s 工具参数不是空对象字符串：%s", name, out[name])
		}
	}
}

// ---- 工具定义没有 parameters / input_schema ----

func TestToolWithoutSchemaOmitsSlotInsteadOfNull(t *testing.T) {
	req := r98Decode(t, "openai-chat", `{"model":"m","max_tokens":16,
		"tools":[{"type":"function","function":{"name":"f","description":"d"}}],
		"messages":[{"role":"user","content":"hi"}]}`)
	out := r98EncodeAll(t, req)
	r98AssertNoNull(t, out, `"parameters":null`, `"input_schema":null`)
	// 工具本身不能跟着消失：客户端少发一个 schema 不等于没声明工具。
	for _, name := range r98Outbound {
		if !strings.Contains(out[name], `"f"`) || !strings.Contains(out[name], `"d"`) {
			t.Errorf("%s 工具定义整条丢失：%s", name, out[name])
		}
	}
}

// anthropic 侧的扁平工具形态（Cursor 那类客户端直接发 input_schema）同样不能凭空
// 多出 null——这条走的是 openaichat 的扁平 DTO 分支。
func TestFlatAnthropicStyleToolWithoutSchemaOmitsSlot(t *testing.T) {
	req := r98Decode(t, "openai-chat", `{"model":"m","max_tokens":16,
		"tools":[{"name":"f","description":"d"}],
		"messages":[{"role":"user","content":"hi"}]}`)
	out := r98EncodeAll(t, req)
	r98AssertNoNull(t, out, `"parameters":null`, `"input_schema":null`)
	for _, name := range r98Outbound {
		if !strings.Contains(out[name], `"f"`) {
			t.Errorf("%s 扁平工具定义丢失：%s", name, out[name])
		}
	}
}

// ---- responses 专属槽位 ----

func TestCustomToolWithoutFormatOmitsSlot(t *testing.T) {
	req := r98Decode(t, "openai-responses", `{"model":"m","max_output_tokens":16,
		"tools":[{"type":"custom","name":"g","description":"d"}],
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	out := r98EncodeAll(t, req)
	r98AssertNoNull(t, out, `"format":null`, `"parameters":null`)
	for _, name := range []string{"codex", "openai-responses"} {
		if !strings.Contains(out[name], `"custom"`) {
			t.Errorf("%s custom 工具没有按原形态回吐：%s", name, out[name])
		}
	}
}

func TestPromptRefWithoutVariablesOmitsSlot(t *testing.T) {
	req := r98Decode(t, "openai-responses", `{"model":"m","max_output_tokens":16,
		"prompt":{"id":"p","version":"1"},
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	out := r98EncodeAll(t, req)
	r98AssertNoNull(t, out, `"variables":null`)
	for _, name := range []string{"codex", "openai-responses"} {
		if !strings.Contains(out[name], `"id":"p"`) {
			t.Errorf("%s prompt 引用丢失：%s", name, out[name])
		}
	}
}

// ---- anthropic 专属槽位 ----

func TestServerToolUseWithoutInputOmitsSlot(t *testing.T) {
	req := r98Decode(t, "anthropic", `{"model":"m","max_tokens":16,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"server_tool_use","id":"s1","name":"web_search"}]}]}`)
	if req.Messages[1].Content[0].ServerToolUse == nil {
		t.Fatalf("server_tool_use 没有解成对应块：%+v", req.Messages[1].Content[0])
	}
	if req.Messages[1].Content[0].ServerToolUse.Input != nil {
		t.Fatalf("入站就把缺省的 input 填成了 %s", req.Messages[1].Content[0].ServerToolUse.Input)
	}
	out := r98EncodeAll(t, req)
	r98AssertNoNull(t, out, `"input":null`)
	if !strings.Contains(out["anthropic"], `"server_tool_use"`) {
		t.Errorf("anthropic 同族往返丢了 server_tool_use 块：%s", out["anthropic"])
	}
}

// ---- 结构化输出只给 type 不给 schema ----

func TestResponseFormatWithoutSchemaStaysJsonMode(t *testing.T) {
	req := r98Decode(t, "openai-chat", `{"model":"m","max_tokens":16,
		"response_format":{"type":"json_object"},
		"messages":[{"role":"user","content":"hi"}]}`)
	if req.ResponseFormat == nil || req.ResponseFormat.Schema != nil {
		t.Fatalf("入站形状不对：%+v", req.ResponseFormat)
	}
	if req.ResponseFormat.IsSchema() {
		t.Fatal("只要求合法 JSON 的请求被判成带 schema")
	}
	out := r98EncodeAll(t, req)
	r98AssertNoNull(t, out, `"schema":null`, `"json_schema":null`, `"parameters":null`)
	if !strings.Contains(out["openai-chat"], `"json_object"`) {
		t.Errorf("chat 同族往返丢了 json 模式：%s", out["openai-chat"])
	}
}

// 反向：显式给了 schema 就必须逐字节活着穿过 Clone。
func TestResponseFormatSchemaSurvivesClone(t *testing.T) {
	const schema = `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`
	req := r98Decode(t, "openai-chat", `{"model":"m","max_tokens":16,
		"response_format":{"type":"json_schema","json_schema":{"name":"n","strict":true,"schema":`+schema+`}},
		"messages":[{"role":"user","content":"hi"}]}`)
	out := r98EncodeAll(t, req)
	for _, name := range r98Outbound {
		if !strings.Contains(out[name], `"required"`) {
			t.Errorf("%s schema 内容丢失：%s", name, out[name])
		}
	}
}
