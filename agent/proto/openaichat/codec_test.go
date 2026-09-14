// codec_test.go openaichat 解码测试：Cursor 扁平工具格式兼容与标准形态回归。
package openaichat

import (
	"encoding/json"
	"strings"
	"testing"
)

// 扁平 + 标准混合数组：扁平 {name,description,input_schema} 映射为 function 工具。
func TestDecodeRequestFlatTools(t *testing.T) {
	body := `{
		"model": "m",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"name": "get_weather", "description": "flat tool", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}},
			{"type": "function", "function": {"name": "std_tool", "description": "standard", "parameters": {"type": "object"}}}
		]
	}`
	req, err := (codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(req.Tools))
	}
	flat, std := req.Tools[0], req.Tools[1]
	if flat.Name != "get_weather" || flat.Description != "flat tool" ||
		!strings.Contains(string(flat.InputSchema), `"city"`) {
		t.Errorf("flat tool decoded wrong: %+v", flat)
	}
	if std.Name != "std_tool" || std.Description != "standard" {
		t.Errorf("standard tool decoded wrong: %+v", std)
	}
}

// 工具名是唯一必需键：仅 {name} 的极简扁平形态也要接受。
func TestDecodeRequestMinimalFlatTool(t *testing.T) {
	req, err := (codec{}).DecodeRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"ping"}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "ping" {
		t.Fatalf("tools = %+v", req.Tools)
	}
}

// 孤儿/错序 tool 消息降级为 user 文本；合法并行 tool 序列原样保留。
func TestEncodeRequestFixToolOrder(t *testing.T) {
	assistantTC := `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`
	cases := []struct {
		name     string
		messages string
		wantTool int
	}{
		{"orphan after user", `[{"role":"user","content":"hi"},` + assistantTC +
			`,{"role":"user","content":"next"},{"role":"tool","tool_call_id":"c1","content":"res"}]`, 0},
		{"paired kept", `[` + assistantTC + `,{"role":"tool","tool_call_id":"c1","content":"res"}]`, 1},
		{"parallel kept", `[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"r1"},{"role":"tool","tool_call_id":"c2","content":"r2"}]`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","tools":[{"name":"f"},{"name":"g"}],"messages":` + tc.messages + `}`
			req, err := (codec{}).DecodeRequest([]byte(body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			out, err := (codec{}).EncodeRequest(req)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var parsed struct {
				Messages []struct {
					Role       string          `json:"role"`
					Content    json.RawMessage `json:"content"`
					ToolCallID string          `json:"tool_call_id"`
					ToolCalls  []struct {
						ID string `json:"id"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("unmarshal encoded: %v", err)
			}
			got := 0
			for i, m := range parsed.Messages {
				if m.Role != "tool" {
					continue
				}
				got++
				gov := -1
				for j := i - 1; j >= 0; j-- {
					if parsed.Messages[j].Role != "tool" {
						gov = j
						break
					}
				}
				if gov < 0 || parsed.Messages[gov].Role != "assistant" {
					t.Errorf("tool message at %d not governed by assistant", i)
					continue
				}
				covered := false
				for _, tc := range parsed.Messages[gov].ToolCalls {
					if tc.ID == m.ToolCallID {
						covered = true
					}
				}
				if !covered {
					t.Errorf("tool message at %d id %s not in governor tool_calls", i, m.ToolCallID)
				}
			}
			if got != tc.wantTool {
				t.Errorf("tool messages = %d, want %d", got, tc.wantTool)
			}
		})
	}
}

// 标准形态回归：无 tools / 标准工具不受 UnmarshalJSON 影响且可再编码。
func TestDecodeRequestStandardToolsRoundTrip(t *testing.T) {
	req, err := (codec{}).DecodeRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"t1","description":"d","parameters":{"type":"object"}}}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "t1" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	// 无 tools 请求回归
	if _, err := (codec{}).DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("no-tools decode: %v", err)
	}
}

// 垃圾工具条目仍报错（既非标准也非扁平）。
func TestDecodeRequestInvalidTool(t *testing.T) {
	_, err := (codec{}).DecodeRequest([]byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"foo":1}]}`))
	if err == nil || !strings.Contains(err.Error(), "tool") {
		t.Fatalf("err = %v, want tool parse error", err)
	}
}

// 扁平形态在请求层之外也要能直接解（DTO 层单测）。
func TestToolUnmarshalFlatDirect(t *testing.T) {
	var tl tool
	if err := json.Unmarshal([]byte(`{"name":"n","description":"d","input_schema":{"type":"object"}}`), &tl); err != nil {
		t.Fatalf("unmarshal flat: %v", err)
	}
	if tl.Type != "function" || tl.Function.Name != "n" || tl.Function.Description != "d" {
		t.Errorf("tool = %+v", tl)
	}
}
