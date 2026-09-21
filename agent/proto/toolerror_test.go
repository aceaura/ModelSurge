package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// 「这次工具调用失败了」在 Anthropic 是 is_error、kiro 是 status=error、
// Gemini 靠 response 里的 error 键。丢掉它模型会把报错文本当成正常返回值。

func errToolResultReq() *ir.Request {
	return &ir.Request{
		Model: "m",
		// 必须声明工具：未声明时 codec 会把工具块降级成纯文本，
		// 失败标志就无从谈起（那是另一条既有规则，不是本轮要测的）。
		Tools: []ir.Tool{{Name: "run", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "check it"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{
				Type:    ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "run", Input: json.RawMessage(`{}`)},
			}}},
			{Role: ir.RoleUser, Content: []ir.Block{{
				Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{
					ToolUseID: "toolu_1", IsError: true,
					Content: []ir.Block{{Type: ir.BlockText, Text: "permission denied"}},
				},
			}}},
		},
	}
}

// 能表达失败的协议必须把标志写到线上。
func TestToolResultErrorReachesWire(t *testing.T) {
	for _, name := range []string{"anthropic", "kiro"} {
		t.Run(name, func(t *testing.T) {
			c, err := proto.GetOutbound(name)
			if err != nil {
				t.Fatalf("GetOutbound(%s): %v", name, err)
			}
			if !c.Caps().ToolResultError {
				t.Fatalf("%s 应声明能表达工具失败", name)
			}
			body, err := c.EncodeRequest(errToolResultReq())
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			s := string(body)
			if !strings.Contains(s, "is_error") && !strings.Contains(s, "error") {
				t.Errorf("线上没有失败标志：%s", s)
			}
			if !strings.Contains(s, "permission denied") {
				t.Errorf("结果文本丢失：%s", s)
			}
		})
	}
}

// 成功的结果不得被标成失败：反向断言，否则「恒填 error」也能通过上面那条。
func TestToolResultSuccessNotMarkedError(t *testing.T) {
	req := errToolResultReq()
	req.Messages[2].Content[0].ToolResult.IsError = false
	for _, name := range []string{"anthropic", "kiro"} {
		t.Run(name, func(t *testing.T) {
			c, _ := proto.GetOutbound(name)
			body, err := c.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			if strings.Contains(string(body), `"is_error":true`) || strings.Contains(string(body), `"status":"error"`) {
				t.Errorf("成功结果被标成失败：%s", body)
			}
		})
	}
}

// Anthropic 解码侧：is_error 必须进 IR，否则下游无从得知。
func TestAnthropicDecodesIsError(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,
		 "content":[{"type":"text","text":"boom"}]}]}]}`)
	req, err := proto.MustInbound("anthropic").DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	tr := req.Messages[0].Content[0].ToolResult
	if tr == nil || !tr.IsError {
		t.Fatalf("is_error 未进 IR：%+v", req.Messages[0].Content[0])
	}
}

// Gemini 没有 is_error 字段，官方约定把失败写成 response 里的 error 键。
func TestGeminiDecodesErrorConvention(t *testing.T) {
	cases := []struct {
		name string
		// funcResp 整个 functionResponse 对象：response 字段可以整个缺席，
		// 所以变量是外层对象而不是它的值。
		funcResp string
		want     bool
	}{
		{"error-object", `{"name":"run","id":"toolu_1","response":{"error":{"message":"boom"}}}`, true},
		{"error-string", `{"name":"run","id":"toolu_1","response":{"error":"boom"}}`, true},
		{"error-null", `{"name":"run","id":"toolu_1","response":{"error":null}}`, false},
		{"error-false", `{"name":"run","id":"toolu_1","response":{"error":false}}`, false},
		{"plain-result", `{"name":"run","id":"toolu_1","response":{"result":"ok"}}`, false},
		{"empty-object", `{"name":"run","id":"toolu_1","response":{}}`, false},
		// response 字段整个缺席：无从判断，不能凭空判成失败。
		{"response-absent", `{"name":"run","id":"toolu_1"}`, false},
		// 非对象形态同样无从判断。
		{"not-an-object", `{"name":"run","id":"toolu_1","response":"just text"}`, false},
		{"response-null", `{"name":"run","id":"toolu_1","response":null}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":` + c.funcResp + `}]}]}`)
			req, err := proto.MustInbound("gemini").DecodeRequest(body)
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			tr := req.Messages[0].Content[0].ToolResult
			if tr == nil {
				t.Fatalf("没有解出 tool_result：%+v", req.Messages[0].Content)
			}
			if tr.IsError != c.want {
				t.Errorf("IsError = %v, want %v（functionResponse=%s）", tr.IsError, c.want, c.funcResp)
			}
		})
	}
}

// OpenAI 两系的载荷里没有失败标志位，能力位必须如实为假——
// 声明为真会让诊断分支恒假，静默丢失就再也报不出来。
func TestOpenAIProtocolsDeclareNoToolResultError(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses"} {
		c, err := proto.GetOutbound(name)
		if err != nil {
			t.Fatalf("GetOutbound(%s): %v", name, err)
		}
		if c.Caps().ToolResultError {
			t.Errorf("%s 的载荷里没有失败标志位，不应声明为真", name)
		}
	}
}
