package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R109-A8 流式 chunk obfuscation 计数注记：上游的侧信道防护随机串过不了
// 中继，客户端看到的是未填充流，帧数报得出；没带该键的流闭嘴。
func TestR109ObfuscationNote(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",`+
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}],"obfuscation":"r4nd0m"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Feed("", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",`+
		`"choices":[{"index":0,"delta":{"content":"!"}}],"obfuscation":"x"}`); err != nil {
		t.Fatal(err)
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "stripped obfuscation padding from 2 chunk(s)") {
		t.Fatalf("obfuscation 注记缺失：%q", joined)
	}

	quiet := New().NewStreamDecoder()
	if _, err := quiet.Feed("", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",`+
		`"choices":[{"index":0,"delta":{"content":"hi"}}]}`); err != nil {
		t.Fatal(err)
	}
	if n := quiet.(interface{ Notes() []string }).Notes(); len(n) != 0 {
		t.Fatalf("无 obfuscation 误报：%q", n)
	}
}

// R109-B3 非流式 moderation 结果注记：与流式 droppedModeration 同判据，
// 响应体的审核结论没有 IR 槽位，丢弃必须报得出。
func TestR109ModerationNoteNonStream(t *testing.T) {
	body := []byte(`{"id":"r1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"moderation":{"results":[{"flagged":false}]}}`)
	_, notes, err := (codec{}).DecodeResponseWithNotes(body)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(notes, "; ")
	if !strings.Contains(joined, "moderation") {
		t.Fatalf("moderation 注记缺失：%q", joined)
	}

	// 没带 moderation 的响应不造注记。
	_, notes2, err := (codec{}).DecodeResponseWithNotes([]byte(`{"id":"r1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes2 {
		if strings.Contains(n, "moderation") {
			t.Fatalf("无 moderation 误报：%q", n)
		}
	}
}

// R109-B9 废弃 functions/function_call 折进现代槽位：功能等价无需注记；
// 现代键同给时现代键胜出；指名形态 {"name":"x"} 走扁平回落。
func TestR109DeprecatedFunctionsFoldIn(t *testing.T) {
	req, err := New().DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"functions":[{"name":"legacy_fn","description":"d","parameters":{"type":"object"}}],` +
		`"function_call":{"name":"legacy_fn"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "legacy_fn" {
		t.Fatalf("functions 没折进 Tools：%+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != ir.ChoiceTool || req.ToolChoice.ToolName != "legacy_fn" {
		t.Fatalf("function_call 指名没折进 ToolChoice：%+v", req.ToolChoice)
	}
	// 编码只产出现代键，不回吐废弃键。
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `"functions"`) || strings.Contains(s, `"function_call"`) {
		t.Fatalf("编码吐出了废弃键：%s", s)
	}
	if !strings.Contains(s, `"tools"`) || !strings.Contains(s, `"tool_choice"`) {
		t.Fatalf("现代键缺失：%s", s)
	}

	// function_call 的字符串形态（"none"/"auto"）同值集生效。
	req2, err := New().DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"functions":[{"name":"f"}],"function_call":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req2.ToolChoice == nil || req2.ToolChoice.Mode != ir.ChoiceNone {
		t.Fatalf(`function_call:"none" 未生效：%+v`, req2.ToolChoice)
	}

	// 现代键同给时现代键胜出（tools 取代 functions，tool_choice 取代 function_call）。
	req3, err := New().DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"functions":[{"name":"legacy_fn"}],"function_call":"none",` +
		`"tools":[{"type":"function","function":{"name":"modern_fn"}}],"tool_choice":"auto"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req3.Tools) != 2 || req3.Tools[0].Name != "modern_fn" {
		t.Fatalf("现代 tools 应在前：%+v", req3.Tools)
	}
	if req3.ToolChoice == nil || req3.ToolChoice.Mode != ir.ChoiceAuto {
		t.Fatalf("现代 tool_choice 应胜出：%+v", req3.ToolChoice)
	}
}
