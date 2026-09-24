package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R107-甲4 chat 本族原生 custom tool call（type=custom，custom{name,input}）：
// 此前只读 function 槽位，custom 调用被伪造成空名函数调用。
func TestCustomToolCallDecode(t *testing.T) {
	// 请求侧历史
	body := `{"model":"gpt-x","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"call_1","type":"custom","custom":{"name":"shell","input":"echo hi"}}]}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var tu *ir.ToolUse
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse {
				tu = b.ToolUse
			}
		}
	}
	if tu == nil || tu.Kind != ir.ToolCustom || tu.Name != "shell" || tu.InputText != "echo hi" {
		t.Fatalf("custom call decoded wrong: %+v", tu)
	}

	// 非流式响应侧同款
	respBody := `{"id":"c1","model":"gpt-x","choices":[{"index":0,"finish_reason":"tool_calls",` +
		`"message":{"role":"assistant","tool_calls":[{"id":"call_9","type":"custom","custom":{"name":"shell","input":"ls"}}]}}]}`
	resp, err := New().DecodeResponse([]byte(respBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil ||
		resp.Content[0].ToolUse.Kind != ir.ToolCustom || resp.Content[0].ToolUse.Name != "shell" ||
		resp.Content[0].ToolUse.InputText != "ls" {
		t.Fatalf("response custom call decoded wrong: %+v", resp.Content)
	}
}

// R107-甲4 编码侧：custom 调用回本族原生形态，不得投影成函数、不得带空
// function 键；函数调用形态不变。
func TestCustomToolCallEncodeNative(t *testing.T) {
	req := &ir.Request{Model: "gpt-x", Tools: []ir.Tool{
		{Name: "shell", Kind: ir.ToolCustom, Description: "run"},
		{Name: "get", InputSchema: []byte(`{"type":"object"}`)},
	}, Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "call_1", Name: "shell", Kind: ir.ToolCustom, InputText: "echo hi",
			Input: []byte(`{"input":"echo hi"}`)}},
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "call_2", Name: "get", Input: []byte(`{"q":1}`)}},
	}}}}
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"custom","custom":{"name":"shell","input":"echo hi"}`) {
		t.Errorf("custom call not encoded natively: %s", s)
	}
	if strings.Contains(s, `"custom":{"name":"shell","input":"echo hi"},"function"`) ||
		strings.Contains(s, `"function":{"name":"","arguments":""}`) {
		t.Errorf("custom call leaked empty function key: %s", s)
	}
	if !strings.Contains(s, `"type":"function","function":{"name":"get","arguments":"{\"q\":1}"}`) {
		t.Errorf("function call form changed: %s", s)
	}
}

// R107-甲8 废弃 function_call 三处不再静默蒸发：请求/非流式载荷完整直接
// 进 IR；流式碎片并入既有 pending 轨道，id 收尾合成。
func TestDeprecatedFunctionCallDecode(t *testing.T) {
	// 请求侧历史
	body := `{"model":"gpt-x","messages":[{"role":"assistant","function_call":{"name":"legacy","arguments":"{\"a\":1}"}}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var name, args string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				name, args = b.ToolUse.Name, string(b.ToolUse.Input)
			}
		}
	}
	if name != "legacy" || args != `{"a":1}` {
		t.Fatalf("request deprecated function_call dropped: %q %q", name, args)
	}

	// 非流式响应侧
	respBody := `{"id":"c1","model":"gpt-x","choices":[{"index":0,"finish_reason":"function_call",` +
		`"message":{"role":"assistant","function_call":{"name":"legacy","arguments":"{\"b\":2}"}}}]}`
	resp, err := New().DecodeResponse([]byte(respBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil || resp.Content[0].ToolUse.Name != "legacy" {
		t.Fatalf("response deprecated function_call dropped: %+v", resp.Content)
	}

	// 对照组：tool_calls 优先，两个槽位同在不重复进 IR。
	both := `{"model":"gpt-x","messages":[{"role":"assistant",` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"modern","arguments":"{}"}}],` +
		`"function_call":{"name":"legacy","arguments":"{}"}}]}`
	req2, err := New().DecodeRequest([]byte(both))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range req2.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse {
				n++
				if b.ToolUse.Name != "modern" {
					t.Errorf("tool_calls 应优先：%q", b.ToolUse.Name)
				}
			}
		}
	}
	if n != 1 {
		t.Errorf("两槽位同在应只进一条：%d", n)
	}
}

// R107-甲8 流式碎片：name/arguments 聚合成完整调用，id 收尾合成并注记。
func TestDeprecatedFunctionCallStream(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	var evs []ir.Event
	for _, data := range []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"function_call":{"name":"legacy"}}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"function_call":{"arguments":"{\"a\":"}}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"function_call":{"arguments":"1}"}}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`,
	} {
		out, err := dec.Feed("data", data)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, out...)
	}
	evs = append(evs, dec.Finish()...)
	var name, args string
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block != nil && ev.Block.ToolUse != nil {
			name = ev.Block.ToolUse.Name
		}
		if ev.Type == ir.EvToolInput {
			args += ev.Text
		}
	}
	if name != "legacy" || args != `{"a":1}` {
		t.Errorf("stream deprecated function_call lost: name=%q args=%q", name, args)
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "synthesized an id") {
		t.Errorf("合成 id 注记不对：%q", notes)
	}
}
