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
