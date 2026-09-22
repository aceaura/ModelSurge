// codec_test.go openaichat 解码测试：Cursor 扁平工具格式兼容与标准形态回归。
package openaichat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func TestDecodeResponseKeepsPrimaryChoice(t *testing.T) {
	body := []byte(`{"id":"chatcmpl_1","model":"m","choices":[{"index":1,"message":{"role":"assistant","content":"B"},"finish_reason":"length"},{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	resp, notes, err := (codec{}).DecodeResponseWithNotes(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "A" {
		t.Fatalf("content = %+v, want choice index 0", resp.Content)
	}
	if resp.StopReason != ir.StopEndTurn {
		t.Fatalf("stop = %q, want %q", resp.StopReason, ir.StopEndTurn)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "discarded 1 additional response choice") {
		t.Fatalf("notes = %v", notes)
	}

	fallback, fallbackNotes, err := (codec{}).DecodeResponseWithNotes([]byte(`{"choices":[{"index":4,"message":{"content":"D"}},{"index":2,"message":{"content":"C"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(fallback.Content) != 1 || fallback.Content[0].Text != "C" {
		t.Fatalf("fallback content = %+v, want smallest choice index", fallback.Content)
	}
	if len(fallbackNotes) != 1 {
		t.Fatalf("fallback notes = %v", fallbackNotes)
	}
}

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

// 错序 tool 回复重排回 governing assistant 旁（内容保留）；合法并行 tool 序列原样保留。
func TestEncodeRequestFixToolOrder(t *testing.T) {
	assistantTC := `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`
	cases := []struct {
		name            string
		messages        string
		wantTool        int
		wantRoles       []string
		wantToolContent string
	}{
		{"misplaced reordered", `[{"role":"user","content":"hi"},` + assistantTC +
			`,{"role":"user","content":"next"},{"role":"tool","tool_call_id":"c1","content":"res"}]`, 1,
			[]string{"user", "assistant", "tool", "user"}, "res"},
		{"paired kept", `[` + assistantTC + `,{"role":"tool","tool_call_id":"c1","content":"res"}]`, 1,
			[]string{"assistant", "tool"}, "res"},
		{"parallel kept", `[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"g","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"r1"},{"role":"tool","tool_call_id":"c2","content":"r2"}]`, 2,
			[]string{"assistant", "tool", "tool"}, ""},
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
				if tc.wantToolContent != "" {
					var content string
					if err := json.Unmarshal(m.Content, &content); err != nil || content != tc.wantToolContent {
						t.Errorf("tool content at %d = %q, want %q (err %v)", i, content, tc.wantToolContent, err)
					}
				}
			}
			if got != tc.wantTool {
				t.Errorf("tool messages = %d, want %d", got, tc.wantTool)
			}
			if tc.wantRoles != nil {
				var roles []string
				for _, m := range parsed.Messages {
					roles = append(roles, m.Role)
				}
				if strings.Join(roles, ",") != strings.Join(tc.wantRoles, ",") {
					t.Errorf("roles = %v, want %v", roles, tc.wantRoles)
				}
			}
		})
	}
}

// fixToolOrder 直测：重复 id 丢弃、孤儿降级（媒体跟随）、媒体压组尾、未归属媒体原位保留。
func TestFixToolOrder(t *testing.T) {
	assistant := func(ids ...string) message {
		var calls []toolCall
		for i, id := range ids {
			calls = append(calls, toolCall{Index: i, ID: id, Type: "function", Function: functionCall{Name: "f", Arguments: "{}"}})
		}
		return message{Role: "assistant", ToolCalls: calls}
	}
	toolMsg := func(id, text string) message {
		return message{Role: "tool", ToolCallID: id, Content: json.RawMessage(marshalString(text))}
	}
	mediaMsg := message{Role: "user",
		Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`), media: true}

	t.Run("duplicate dropped", func(t *testing.T) {
		got := fixToolOrder([]message{assistant("c1"), toolMsg("c1", "r1"), toolMsg("c1", "r2")})
		if len(got) != 2 || got[0].Role != "assistant" || got[1].Role != "tool" || string(got[1].Content) != `"r1"` {
			t.Fatalf("got = %+v", got)
		}
	})
	t.Run("orphan downgraded with media", func(t *testing.T) {
		got := fixToolOrder([]message{toolMsg("cX", "res"), mediaMsg})
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2: %+v", len(got), got)
		}
		var text string
		if err := json.Unmarshal(got[0].Content, &text); err != nil || text != "[Tool Result (cX)]\nres" {
			t.Errorf("downgraded content = %q (err %v)", text, err)
		}
		if !got[1].media || got[1].Role != "user" {
			t.Errorf("orphan media should follow downgraded text: %+v", got[1])
		}
	})
	t.Run("media pressed to group tail", func(t *testing.T) {
		got := fixToolOrder([]message{assistant("c1", "c2"), toolMsg("c1", "r1"), mediaMsg, toolMsg("c2", "r2"), mediaMsg})
		want := "assistant,tool,tool,user,user"
		var roles []string
		for _, m := range got {
			roles = append(roles, m.Role)
		}
		if strings.Join(roles, ",") != want {
			t.Fatalf("roles = %v, want %s", roles, want)
		}
		if !got[3].media || !got[4].media {
			t.Errorf("tail messages should be media: %+v", got[3:])
		}
	})
	t.Run("unowned media kept in place", func(t *testing.T) {
		got := fixToolOrder([]message{{Role: "user", Content: json.RawMessage(`"hi"`)}, mediaMsg})
		if len(got) != 2 || !got[1].media {
			t.Fatalf("got = %+v", got)
		}
	})
}

// tool 结果内嵌图片：tool 消息只承载文本，图片抽出为媒体 user 消息压在回复组之后。
func TestEncodeRequestToolResultMedia(t *testing.T) {
	body := `{"model":"m","tools":[{"name":"f"}],"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":[
			{"type":"text","text":"see"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`
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
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal encoded: %v", err)
	}
	if len(parsed.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %s", len(parsed.Messages), out)
	}
	if m := parsed.Messages[2]; m.Role != "tool" || m.ToolCallID != "c1" || string(m.Content) != `"see"` {
		t.Errorf("tool message = %+v content %s", m, m.Content)
	}
	var parts []part
	if err := json.Unmarshal(parsed.Messages[3].Content, &parts); err != nil || len(parts) != 1 ||
		parts[0].Type != "image_url" || parts[0].ImageURL == nil || parts[0].ImageURL.URL != "data:image/png;base64,QUJD" {
		t.Errorf("media message parts = %+v (err %v)", parts, err)
	}
}

// thinking 开启时强制 tool_choice（required/指定函数）降级 auto；思考关闭则保留。
func TestEncodeRequestThinkingToolChoice(t *testing.T) {
	run := func(t *testing.T, body string) any {
		t.Helper()
		req, err := (codec{}).DecodeRequest([]byte(body))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		out, err := (codec{}).EncodeRequest(req)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var parsed struct {
			ToolChoice any `json:"tool_choice"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("unmarshal encoded: %v", err)
		}
		if parsed.ToolChoice == nil {
			t.Fatalf("tool_choice missing: %s", out)
		}
		return parsed.ToolChoice
	}
	head := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f"}]`
	if tc := run(t, head+`,"reasoning_effort":"high","tool_choice":"required"}`); tc != "auto" {
		t.Errorf("thinking + required = %v, want auto", tc)
	}
	if tc := run(t, head+`,"reasoning_effort":"high","tool_choice":{"type":"function","function":{"name":"f"}}}`); tc != "auto" {
		t.Errorf("thinking + named = %v, want auto", tc)
	}
	if tc := run(t, head+`,"tool_choice":"required"}`); tc != "required" {
		t.Errorf("no thinking + required = %v, want required", tc)
	}
	if tc := run(t, head+`,"reasoning_effort":"none","tool_choice":"required"}`); tc != "required" {
		t.Errorf("effort none + required = %v, want required", tc)
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
