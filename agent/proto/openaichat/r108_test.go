package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R108-乙2 chat tool_choice 的 allowed_tools 嵌套形态：此前整条被丢，
// 连内层 required 一起没了。现在 mode 与工具名白名单都进 IR。
func TestR108AllowedToolsChoiceDecode(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather"}},{"type":"function","function":{"name":"get_time"}}],` +
		`"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[` +
		`{"type":"function","function":{"name":"get_weather"}}]}}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	tc := req.ToolChoice
	if tc == nil || tc.Mode != ir.ChoiceAny {
		t.Fatalf("Mode = %+v", tc)
	}
	if len(tc.AllowedTools) != 1 || tc.AllowedTools[0] != "get_weather" {
		t.Fatalf("AllowedTools = %v", tc.AllowedTools)
	}

	// auto 模式保持 auto。
	body2 := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":["get_time"]}}}`)
	req2, err := New().DecodeRequest(body2)
	if err != nil {
		t.Fatal(err)
	}
	if req2.ToolChoice == nil || req2.ToolChoice.Mode != ir.ChoiceAuto {
		t.Fatalf("auto Mode = %+v", req2.ToolChoice)
	}
}

// R108-乙2 custom 指名双向：{"type":"custom","custom":{"name":...}} 解码成
// ChoiceTool+ToolCustom，同族编码写回 custom 形态（不是 function）。
// tools 里放普通 function：本条只保 tool_choice 形态，custom 工具定义
// 本身的翻译是另一个独立缺口。
func TestR108CustomToolChoiceRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"shell","parameters":{"type":"object"}}}],` +
		`"tool_choice":{"type":"custom","custom":{"name":"shell"}}}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	tc := req.ToolChoice
	if tc == nil || tc.Mode != ir.ChoiceTool || tc.ToolName != "shell" || tc.ToolKind != ir.ToolCustom {
		t.Fatalf("ToolChoice = %+v", tc)
	}
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"tool_choice":{"type":"custom","custom":{"name":"shell"}}`) {
		t.Fatalf("custom 指名没回写：%s", out)
	}
}

// R108-乙4 max_completion_tokens 来路键名带回：客户端给现代键就回现代键，
// 不换写成官方注明不兼容 o 系的废弃 max_tokens；给旧键则维持旧键。
func TestR108MaxCompletionKeyEcho(t *testing.T) {
	modern, err := New().DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":111}`))
	if err != nil {
		t.Fatal(err)
	}
	if !modern.MaxCompletionKey || modern.MaxTokens != 111 {
		t.Fatalf("modern: key=%v tokens=%d", modern.MaxCompletionKey, modern.MaxTokens)
	}
	out, err := New().EncodeRequest(modern)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"max_completion_tokens":111`) || strings.Contains(string(out), `"max_tokens"`) {
		t.Fatalf("现代键没带回：%s", out)
	}

	legacy, err := New().DecodeRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":77}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.MaxCompletionKey || legacy.MaxTokens != 77 {
		t.Fatalf("legacy: key=%v tokens=%d", legacy.MaxCompletionKey, legacy.MaxTokens)
	}
	out2, err := New().EncodeRequest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), `"max_tokens":77`) || strings.Contains(string(out2), "max_completion_tokens") {
		t.Fatalf("旧键没维持：%s", out2)
	}
}

// R108-乙3 chat 非流式 metadata 回显往返：客户端的请求-响应关联数据，
// 不读回就是同族往返把它弄丢。显式 null 按没给处理。
func TestR108MetadataRoundTrip(t *testing.T) {
	resp := []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"metadata":{"trace":"abc"}}`)
	r, err := New().DecodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Metadata) != `{"trace":"abc"}` {
		t.Fatalf("Metadata = %s", r.Metadata)
	}
	out, err := New().EncodeResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"metadata":{"trace":"abc"}`) {
		t.Fatalf("metadata 没回写：%s", out)
	}

	r2, err := New().DecodeResponse([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"metadata":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Metadata) != 0 {
		t.Fatalf("null 被当真值：%s", r2.Metadata)
	}
}

// R108-甲2 流式 chunk 的 moderation 审核结果：没有 IR 槽位，计数经
// Notes() 报出；没给的流静默。
func TestR108ChunkModerationNoted(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",`+
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}],`+
		`"moderation":{"input":{"flagged":false},"output":{"flagged":false}}}`); err != nil {
		t.Fatal(err)
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "moderation") {
		t.Fatalf("notes = %v", notes)
	}
	// 注记一次性：再取为空。
	if again := dec.(interface{ Notes() []string }).Notes(); len(again) != 0 {
		t.Fatalf("注记重复报：%v", again)
	}

	dec2 := New().NewStreamDecoder()
	if _, err := dec2.Feed("", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",`+
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`); err != nil {
		t.Fatal(err)
	}
	if n := dec2.(interface{ Notes() []string }).Notes(); len(n) != 0 {
		t.Fatalf("没给被报：%v", n)
	}
}
