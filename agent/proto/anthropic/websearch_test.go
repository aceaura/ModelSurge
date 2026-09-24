package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// web_search_tool_result.content 是 union：错误形态（web_search_tool_result_error
// 对象）与结果数组二选一。解码必须区分两者——把错误解成零结果等于把「搜索失败」
// 伪造成「搜索成功但没找到东西」。
func TestWebSearchToolResultErrorForm(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":100,"messages":[
		{"role":"user","content":[
			{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1",
			 "content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}},
			{"type":"web_search_tool_result","tool_use_id":"srvtoolu_2","content":[]}]}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	errBlk := req.Messages[0].Content[0].WebSearchToolResult
	if errBlk == nil || errBlk.ErrorCode != "max_uses_exceeded" {
		t.Fatalf("error form = %+v", errBlk)
	}
	if len(errBlk.Results) != 0 {
		t.Errorf("error form must not carry results: %+v", errBlk.Results)
	}
	emptyBlk := req.Messages[0].Content[1].WebSearchToolResult
	if emptyBlk == nil || emptyBlk.ErrorCode != "" || len(emptyBlk.Results) != 0 {
		t.Fatalf("zero-result success must stay distinct from error: %+v", emptyBlk)
	}

	// 同族编码回写：错误形态回到错误对象，零结果回到空数组。
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"`) {
		t.Errorf("error form not re-encoded: %s", s)
	}
	// 错误对象不得同时夹带结果数组形态。
	var wire struct {
		Messages []struct {
			Content []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	got := wire.Messages[0].Content
	var first map[string]any
	if err := json.Unmarshal(got[0].Content, &first); err != nil {
		t.Fatalf("error form content must be an object: %s", got[0].Content)
	}
	var arr []any
	if err := json.Unmarshal(got[1].Content, &arr); err != nil || len(arr) != 0 {
		t.Fatalf("zero-result form content must be an empty array: %s", got[1].Content)
	}
}

// caller 回执与 page_age 是同族往返的可选字段：原样带回，且过 Clone
// （JSON 往返）后仍不丢。
func TestWebSearchToolResultCallerPageAge(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":100,"messages":[
		{"role":"user","content":[
			{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1",
			 "caller":{"type":"server_tool","tool_use_id":"toolu_9"},
			 "content":[{"type":"web_search_result","title":"Go","url":"https://go.dev",
				"encrypted_content":"fast","page_age":"April 15, 2025"}]}]}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	wsr := req.Messages[0].Content[0].WebSearchToolResult
	if string(wsr.Caller) != `{"type":"server_tool","tool_use_id":"toolu_9"}` {
		t.Fatalf("caller = %s", wsr.Caller)
	}
	if wsr.Results[0].PageAge != "April 15, 2025" {
		t.Fatalf("page_age = %+v", wsr.Results[0])
	}

	cloned := req.Clone()
	out, err := New().EncodeRequest(cloned)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"caller":{"type":"server_tool","tool_use_id":"toolu_9"}`, `"page_age":"April 15, 2025"`} {
		if !strings.Contains(s, want) {
			t.Errorf("re-encoded request missing %s: %s", want, s)
		}
	}
}

// 流式响应侧：错误形态结果块随 content_block_start 全量下发时同样保持
// 错误对象形态（与批量编码共用同一 encodeBlock 路径，这里钉住不被回归拆开）。
func TestWebSearchToolResultErrorFormStream(t *testing.T) {
	enc := New().NewStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type: ir.BlockWebSearchToolResult,
		WebSearchToolResult: &ir.WebSearchToolResult{
			ToolUseID: "srvtoolu_3", ErrorCode: "query_too_long"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, fr := range frames {
		out = append(out, fr...)
	}
	if !strings.Contains(string(out), `"type":"web_search_tool_result_error","error_code":"query_too_long"`) {
		t.Errorf("stream block start lost error form: %s", out)
	}
}
