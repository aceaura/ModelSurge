package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// parallelmatrix_test.go 「禁止并行工具调用」的跨协议矩阵。
//
// 此前只有 anthropic 读写 disable_parallel_tool_use，Chat 与 responses 的
// parallel_tool_calls 根本不在 DTO 里：客户端禁止并行，转到 OpenAI 两系后
// 静默变回允许，且无任何诊断。矩阵按「入站表达 -> IR -> 出站字节」验证。

func parallelRequest() *ir.Request {
	return &ir.Request{
		Model:      "m",
		MaxTokens:  1024,
		Messages:   []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:      []ir.Tool{{Name: "t", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceAuto, DisableParallel: true},
	}
}

func encodeOut(t *testing.T, protoName string, req *ir.Request) string {
	t.Helper()
	c, err := proto.GetOutbound(protoName)
	if err != nil {
		t.Fatalf("GetOutbound(%s): %v", protoName, err)
	}
	body, err := c.EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s EncodeRequest: %v", protoName, err)
	}
	return string(body)
}

func decodeIn(t *testing.T, protoName, body string) *ir.Request {
	t.Helper()
	c, err := proto.GetInbound(protoName)
	if err != nil {
		t.Fatalf("GetInbound(%s): %v", protoName, err)
	}
	req, err := c.DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s DecodeRequest: %v", protoName, err)
	}
	return req
}

// 出站：三个能表达该维度的协议都要在字节里写出对应字段。
func TestDisableParallelReachesWire(t *testing.T) {
	cases := map[string]string{
		"anthropic":        `"disable_parallel_tool_use":true`,
		"openai-chat":      `"parallel_tool_calls":false`,
		"openai-responses": `"parallel_tool_calls":false`,
	}
	for name, want := range cases {
		got := encodeOut(t, name, parallelRequest())
		if !strings.Contains(got, want) {
			t.Errorf("%s 未写出 %s：%s", name, want, got)
		}
	}
}

// 客户端没表态时不得替上游发明 true/false —— 默认值归上游。
func TestParallelSilentWhenClientDidNotAsk(t *testing.T) {
	req := parallelRequest()
	req.ToolChoice = &ir.ToolChoice{Mode: ir.ChoiceAuto}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		if got := encodeOut(t, name, req); strings.Contains(got, "parallel_tool_calls") {
			t.Errorf("%s 不应写出 parallel_tool_calls：%s", name, got)
		}
	}
	if got := encodeOut(t, "anthropic", req); strings.Contains(got, "disable_parallel_tool_use") {
		t.Errorf("anthropic 不应写出 disable_parallel_tool_use：%s", got)
	}
	// 完全没有 tool_choice 时同理
	req.ToolChoice = nil
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		got := encodeOut(t, name, req)
		if strings.Contains(got, "parallel_tool_calls") || strings.Contains(got, "disable_parallel_tool_use") {
			t.Errorf("%s 无 tool_choice 时不应写出并行字段：%s", name, got)
		}
	}
}

// 入站：parallel_tool_calls=false 要解成 IR 的 DisableParallel。
func TestParallelDecodedFromInbound(t *testing.T) {
	bodies := map[string]string{
		"openai-chat":      `{"model":"m","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":false}`,
		"openai-responses": `{"model":"m","input":"hi","parallel_tool_calls":false}`,
	}
	for name, body := range bodies {
		req := decodeIn(t, name, body)
		if req.ToolChoice == nil || !req.ToolChoice.DisableParallel {
			t.Errorf("%s 未解出 DisableParallel：%+v", name, req.ToolChoice)
		}
	}
	// true 与缺失都表示允许并行，不应置位（否则会把「允许」翻成「禁止」）
	for name, body := range map[string]string{
		"openai-chat":      `{"model":"m","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":true}`,
		"openai-responses": `{"model":"m","input":"hi","parallel_tool_calls":true}`,
	} {
		if req := decodeIn(t, name, body); req.ToolChoice != nil && req.ToolChoice.DisableParallel {
			t.Errorf("%s: parallel_tool_calls=true 不应置 DisableParallel", name)
		}
	}
	for name, body := range map[string]string{
		"openai-chat":      `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		"openai-responses": `{"model":"m","input":"hi"}`,
	} {
		if req := decodeIn(t, name, body); req.ToolChoice != nil && req.ToolChoice.DisableParallel {
			t.Errorf("%s: 缺字段不应置 DisableParallel", name)
		}
	}
}

// 显式 tool_choice 与并行开关共存时，两者都要保住（并行位不能顶掉 Mode）。
// 必须声明工具：零工具下的 tool_choice:"required" 是上游必拒的形状，规整流水线
// 会把它整条删掉，那样这条断言测的就是删改而不是共存了。
func TestParallelPreservesExplicitToolChoice(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"alpha","parameters":{"type":"object"}}}],` +
		`"tool_choice":"required","parallel_tool_calls":false}`
	req := decodeIn(t, "openai-chat", body)
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ChoiceAny || !req.ToolChoice.DisableParallel {
		t.Fatalf("Mode 与 DisableParallel 应同时保住：%+v", req.ToolChoice)
	}
	out := encodeOut(t, "openai-chat", req)
	if !strings.Contains(out, `"parallel_tool_calls":false`) || !strings.Contains(out, `"tool_choice":"required"`) {
		t.Errorf("往返后两维都应在线：%s", out)
	}
}

// 跨协议往返：Chat 客户端的禁止并行送到 Anthropic 上游要落成
// disable_parallel_tool_use，反向亦然。
func TestDisableParallelCrossProtocolRoundTrip(t *testing.T) {
	fromChat := decodeIn(t, "openai-chat",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":false}`)
	if got := encodeOut(t, "anthropic", fromChat); !strings.Contains(got, `"disable_parallel_tool_use":true`) {
		t.Errorf("chat -> anthropic 丢了禁止并行：%s", got)
	}
	fromAnthropic := decodeIn(t, "anthropic",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`)
	if got := encodeOut(t, "openai-chat", fromAnthropic); !strings.Contains(got, `"parallel_tool_calls":false`) {
		t.Errorf("anthropic -> chat 丢了禁止并行：%s", got)
	}
	if got := encodeOut(t, "openai-responses", fromAnthropic); !strings.Contains(got, `"parallel_tool_calls":false`) {
		t.Errorf("anthropic -> responses 丢了禁止并行：%s", got)
	}
}
