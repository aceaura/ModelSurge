package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func thinkingReq(sig, from string) *ir.Request {
	return &ir.Request{
		Model:     "claude-x",
		MaxTokens: 64,
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm", Signature: sig, SignatureFrom: from}},
				{Type: ir.BlockText, Text: "ok"},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
		},
	}
}

// 解码时签名的协议形态标记为 anthropic。
func TestDecodeRequest_ThinkingSignatureFrom(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":64,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"hmm","signature":"sig"},
			{"type":"text","text":"ok"}]},
		{"role":"user","content":"go"}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	th := req.Messages[1].Content[0].Thinking
	if th.Signature != "sig" || th.SignatureFrom != "anthropic" {
		t.Errorf("thinking = %+v, want sig with SignatureFrom=anthropic", th)
	}
}

// 请求方向：同族签名透传；外族/无签名降级为 text——
// Anthropic 对历史 thinking 块强制签名校验，透传必 400。
func TestEncodeRequest_ThinkingDegradation(t *testing.T) {
	out, err := New().EncodeRequest(thinkingReq("sig", "anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"signature":"sig"`) {
		t.Errorf("same-protocol signature should pass through: %s", out)
	}

	for _, tc := range []struct{ sig, from string }{
		{"sig", "gemini"}, // 外族签名
		{"sig", ""},       // 来源不明的签名
		{"", "anthropic"}, // 无签名
	} {
		out, err := New().EncodeRequest(thinkingReq(tc.sig, tc.from))
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if strings.Contains(s, `"signature"`) {
			t.Errorf("signature (sig=%q from=%q) should be dropped: %s", tc.sig, tc.from, s)
		}
		if strings.Contains(s, `"thinking"`) {
			t.Errorf("unverifiable thinking block (sig=%q from=%q) should degrade to text: %s", tc.sig, tc.from, s)
		}
		if !strings.Contains(s, `"hmm"`) {
			t.Errorf("thinking text (sig=%q from=%q) should survive as text: %s", tc.sig, tc.from, s)
		}
	}
}

// 响应方向：thinking 块不降级为 text（正文照留），但签名位按来源门控——
// 外族签名不得写进 signature 冒充本族真签名，客户端下一轮回传会被上游拒。
func TestEncodeResponse_ThinkingSignatureGatedByOrigin(t *testing.T) {
	enc := func(from string) string {
		out, err := New().EncodeResponse(&ir.Response{
			ID: "msg_1", Model: "claude-x",
			Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "hmm", Signature: "sig", SignatureFrom: from,
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	for _, from := range []string{"gemini", ir.SigSynthetic, ""} {
		s := enc(from)
		if !strings.Contains(s, `"hmm"`) {
			t.Errorf("from=%q 思考正文被丢了：%s", from, s)
		}
		if strings.Contains(s, `"signature"`) {
			t.Errorf("from=%q 外来签名被洗进原生签名位：%s", from, s)
		}
	}
	// 本族真签名必须原样回去，否则会话粘性下的签名链每一轮都断。
	if s := enc(Name); !strings.Contains(s, `"signature":"sig"`) {
		t.Errorf("本族真签名没带回：%s", s)
	}
}

// server_tool_use / web_search_tool_result 块：请求解码（会话重放）与
// 响应编码（块往返）两方向不丢字段。
func TestServerToolBlocks_RoundTrip(t *testing.T) {
	// 请求重放：assistant 含 server_tool_use，user 含 web_search_tool_result
	body := `{"model":"claude-x","max_tokens":64,"messages":[
		{"role":"user","content":"search go"},
		{"role":"assistant","content":[
			{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"go"}},
			{"type":"text","text":"<web_search>results</web_search>"}]},
		{"role":"user","content":[
			{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[
				{"type":"web_search_result","title":"Go","url":"https://go.dev","encrypted_content":"fast"}]},
			{"type":"text","text":"summarize"}]}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	stu := req.Messages[1].Content[0]
	if stu.Type != ir.BlockServerToolUse || stu.ServerToolUse == nil {
		t.Fatalf("server_tool_use block = %+v", stu)
	}
	if stu.ServerToolUse.ID != "srvtoolu_1" || stu.ServerToolUse.Name != "web_search" {
		t.Errorf("server_tool_use = %+v", stu.ServerToolUse)
	}
	if string(stu.ServerToolUse.Input) != `{"query":"go"}` {
		t.Errorf("input = %s", stu.ServerToolUse.Input)
	}
	wsr := req.Messages[2].Content[0]
	if wsr.Type != ir.BlockWebSearchToolResult || wsr.WebSearchToolResult == nil {
		t.Fatalf("web_search_tool_result block = %+v", wsr)
	}
	if wsr.WebSearchToolResult.ToolUseID != "srvtoolu_1" || len(wsr.WebSearchToolResult.Results) != 1 {
		t.Fatalf("results = %+v", wsr.WebSearchToolResult)
	}
	r := wsr.WebSearchToolResult.Results[0]
	if r.Title != "Go" || r.URL != "https://go.dev" || r.Snippet != "fast" {
		t.Errorf("result = %+v", r)
	}

	// 响应编码：块按原形态输出
	resp := &ir.Response{
		ID: "msg_1", Model: "claude-x", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_2", Name: "web_search", Input: []byte(`{"query":"go"}`)}},
			{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{
				ToolUseID: "srvtoolu_2",
				Results:   []ir.WebSearchResult{{Title: "Go", URL: "https://go.dev", Snippet: "fast"}}}},
			{Type: ir.BlockText, Text: "summary"},
		},
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"type":"server_tool_use"`,
		`"srvtoolu_2"`,
		`"type":"web_search_tool_result"`,
		`"encrypted_content":"fast"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("response missing %s: %s", want, out)
		}
	}
}

// 流式编码：server_tool_use 块参数经 input_json_delta 下发，
// web_search_tool_result 块 content 全量随块开始下发。
func TestServerToolBlocks_Stream(t *testing.T) {
	enc := New().NewStreamEncoder()
	var out []byte
	feed := func(ev ir.Event) {
		t.Helper()
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("encode %v: %v", ev.Type, err)
		}
		for _, fr := range frames {
			out = append(out, fr...)
		}
	}
	stu := ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
		Type:          ir.BlockServerToolUse,
		ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_3", Name: "web_search", Input: []byte(`{"query":"go"}`)},
	}}
	feed(stu)
	feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"query":"go"}`})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{
		Type: ir.BlockWebSearchToolResult,
		WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "srvtoolu_3",
			Results: []ir.WebSearchResult{{Title: "Go", URL: "https://go.dev", Snippet: "fast"}}},
	}})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 1})
	for _, fr := range enc.Finish() {
		out = append(out, fr...)
	}

	s := string(out)
	for _, want := range []string{
		`"type":"server_tool_use"`,
		`"id":"srvtoolu_3"`,
		`"input":{}`,
		`"partial_json":"{\"query\":\"go\"}"`,
		`"type":"web_search_tool_result"`,
		`"encrypted_content":"fast"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("stream missing %s:\n%s", want, s)
		}
	}
}

// 预算夹紧：强制预算（账号级覆盖常见）超过客户端 max_tokens 时夹紧到 max_tokens-1，
// 避免发出 Anthropic 协议非法请求（budget_tokens 必须 < max_tokens）。
func TestEncodeRequest_ThinkingBudgetClamp(t *testing.T) {
	req := &ir.Request{
		Model:     "m",
		MaxTokens: 2048,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking:  &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096},
	}
	out, err := New().EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"budget_tokens":2047`) {
		t.Errorf("budget not clamped to max_tokens-1: %s", s)
	}
}

// ClampThinking：缺省补默认预算；越界夹紧到 max_tokens-1；未启用不动。
func TestClampThinking(t *testing.T) {
	cl, ok := New().(interface{ ClampThinking(*ir.Request) })
	if !ok {
		t.Fatal("anthropic codec must implement ClampThinking")
	}
	r := &ir.Request{MaxTokens: 2048, Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096}}
	cl.ClampThinking(r)
	if r.Thinking.BudgetTokens != 2047 {
		t.Errorf("budget = %d, want 2047", r.Thinking.BudgetTokens)
	}
	r = &ir.Request{MaxTokens: 8192, Thinking: &ir.ThinkingConfig{Enabled: true}}
	cl.ClampThinking(r)
	if r.Thinking.BudgetTokens != 4096 {
		t.Errorf("default budget = %d, want 4096", r.Thinking.BudgetTokens)
	}
	r = &ir.Request{MaxTokens: 100, Thinking: &ir.ThinkingConfig{Enabled: false, BudgetTokens: 4096}}
	cl.ClampThinking(r)
	if r.Thinking.BudgetTokens != 4096 {
		t.Errorf("disabled thinking must not be touched: %d", r.Thinking.BudgetTokens)
	}
}
