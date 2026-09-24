package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R102-D 解码侧：responses 托管输出项进 IR。
// 三条入路（流式 added/done、added 无 done 的断流兜底、非流式 output 数组、
// 请求历史 input 数组）此前都把 web_search_call 与其它托管 item 静默丢掉。

func feedAll(t *testing.T, dec interface {
	Feed(event, data string) ([]ir.Event, error)
}, frames ...string) []ir.Event {
	t.Helper()
	var out []ir.Event
	for _, f := range frames {
		evs, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("Feed(%s): %v", f, err)
		}
		out = append(out, evs...)
	}
	return out
}

func blockStarts(evs []ir.Event) []ir.Block {
	var out []ir.Block
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block != nil {
			out = append(out, *ev.Block)
		}
	}
	return out
}

const wsAdded = `{"type":"response.output_item.added","output_index":0,` +
	`"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}`

const wsDone = `{"type":"response.output_item.done","output_index":0,` +
	`"item":{"type":"web_search_call","id":"ws_1","status":"completed",` +
	`"action":{"type":"search","query":"weather in Paris",` +
	`"sources":[{"type":"url","url":"https://example.com/p"}]}}}`

// 流式 added/done：web_search_call 合成 server_tool_use + web_search_tool_result
// 配平块；查询串走 EvToolInput 增量通道（开块不带 input，与 anthropic 流式同形）。
func TestStreamDecodesWebSearchCallPair(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec, wsAdded, wsDone)
	var sawInput string
	for _, ev := range evs {
		if ev.Type == ir.EvToolInput {
			sawInput += ev.Text
		}
	}
	if !strings.Contains(sawInput, "weather in Paris") {
		t.Errorf("查询串没走 EvToolInput 通道：events=%+v", evs)
	}
	starts := blockStarts(evs)
	if len(starts) != 2 {
		t.Fatalf("应合成调用+结果两个块，实得 %d：%+v", len(starts), starts)
	}
	call, result := starts[0], starts[1]
	if call.Type != ir.BlockServerToolUse || call.ServerToolUse == nil ||
		call.ServerToolUse.ID != "ws_1" || call.ServerToolUse.Name != "web_search" {
		t.Errorf("调用块形态不对：%+v", call)
	}
	if call.ServerToolUse != nil && len(call.ServerToolUse.Input) != 0 {
		t.Errorf("开块不应带 input（走增量通道）：%s", call.ServerToolUse.Input)
	}
	if result.Type != ir.BlockWebSearchToolResult || result.WebSearchToolResult == nil ||
		result.WebSearchToolResult.ToolUseID != "ws_1" {
		t.Fatalf("结果块形态不对：%+v", result)
	}
	rs := result.WebSearchToolResult.Results
	if len(rs) != 1 || rs[0].URL != "https://example.com/p" {
		t.Errorf("来源没进结果块：%+v", rs)
	}
	if n, ok := dec.(interface{ Notes() []string }); ok {
		if notes := n.Notes(); len(notes) != 0 {
			t.Errorf("web_search_call 已映射，不该有注记：%q", notes)
		}
	}
}

// added 到了 done 没到（断流）：terminalEvents 用 added 帧原文兜底合成调用对，
// 这次托管搜索不许凭空消失。added 帧没有 action：查询未知但调用事实保留。
func TestStreamWebSearchAddedWithoutDone(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec, wsAdded,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	starts := blockStarts(evs)
	if len(starts) != 2 {
		t.Fatalf("断流兜底应合成两个块，实得 %d：%+v", len(starts), starts)
	}
	if starts[0].Type != ir.BlockServerToolUse || starts[0].ServerToolUse.ID != "ws_1" {
		t.Errorf("兜底调用块丢失：%+v", starts[0])
	}
	if starts[1].Type != ir.BlockWebSearchToolResult || starts[1].WebSearchToolResult.ToolUseID != "ws_1" {
		t.Errorf("兜底结果块丢失：%+v", starts[1])
	}
}

// 未映射的托管 item（file_search_call 等）：整块留成不透明块，同族可原样
// 带回；计数经 Notes() 报出且排干归零。注记不得带出 item 载荷（会话内容）。
func TestStreamHostedItemOpaqueAndNotes(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.output_item.added","output_index":0,`+
			`"item":{"type":"file_search_call","id":"fs_1","status":"in_progress"}}`,
		`{"type":"response.output_item.done","output_index":0,`+
			`"item":{"type":"file_search_call","id":"fs_1","status":"completed",`+
			`"queries":["retrieval-x"],"results":[]}}`)
	starts := blockStarts(evs)
	if len(starts) != 1 || starts[0].Type != ir.BlockOpaque || starts[0].Opaque == nil {
		t.Fatalf("托管 item 应归不透明块：%+v", starts)
	}
	o := starts[0].Opaque
	if o.WireType != "file_search_call" || o.From != Name {
		t.Errorf("不透明块判别值不对：%+v", o)
	}
	if !strings.Contains(string(o.Body), "retrieval-x") {
		t.Errorf("item 原文没整块保留：%s", o.Body)
	}
	n := dec.(interface{ Notes() []string })
	notes := n.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "1 hosted output item(s)") {
		t.Fatalf("未映射托管 item 未报注记：%q", notes)
	}
	if strings.Contains(notes[0], "retrieval-x") {
		t.Errorf("注记带出了 item 载荷：%s", notes[0])
	}
	if again := n.Notes(); len(again) != 0 {
		t.Errorf("Notes() 未排干：%q", again)
	}
}

// 未映射托管 item 断流（added 无 done）：terminalEvents 同样兜底成不透明块。
func TestStreamHostedItemAddedWithoutDone(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.output_item.added","output_index":0,`+
			`"item":{"type":"mcp_call","id":"mcp_1","name":"tool-x","status":"in_progress"}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`)
	starts := blockStarts(evs)
	if len(starts) != 1 || starts[0].Type != ir.BlockOpaque ||
		starts[0].Opaque.WireType != "mcp_call" {
		t.Fatalf("断流兜底应归不透明块：%+v", starts)
	}
	if notes := dec.(interface{ Notes() []string }).Notes(); len(notes) != 1 {
		t.Errorf("兜底不透明块未报注记：%q", notes)
	}
}

// 非流式 output 数组：web_search_call 成对、未映射 item 归不透明、正文照常。
func TestDecodeResponseHostedItems(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"m","status":"completed","output":[` +
		`{"type":"web_search_call","id":"ws_1","status":"completed","action":{` +
		`"type":"search","query":"weather in Paris","sources":[{"type":"url","url":"https://example.com/p"}]}},` +
		`{"type":"code_interpreter_call","id":"ci_1","status":"completed","code":"print(1)"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	var call *ir.ServerToolUse
	var result *ir.WebSearchToolResult
	var opaque *ir.Opaque
	var text string
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockServerToolUse:
			call = b.ServerToolUse
		case ir.BlockWebSearchToolResult:
			result = b.WebSearchToolResult
		case ir.BlockOpaque:
			opaque = b.Opaque
		case ir.BlockText:
			text += b.Text
		}
	}
	if call == nil || call.ID != "ws_1" || !strings.Contains(string(call.Input), "weather in Paris") {
		t.Errorf("非流式丢了托管调用：%+v", resp.Content)
	}
	if result == nil || result.ToolUseID != "ws_1" || len(result.Results) != 1 ||
		result.Results[0].URL != "https://example.com/p" {
		t.Errorf("非流式丢了搜索结果：%+v", resp.Content)
	}
	if opaque == nil || opaque.WireType != "code_interpreter_call" ||
		!strings.Contains(string(opaque.Body), "print(1)") {
		t.Errorf("非流式丢了未映射托管 item：%+v", resp.Content)
	}
	if text != "done" {
		t.Errorf("正文被误伤：%q", text)
	}
}

// 请求历史里的 web_search_call：调用块并进 assistant，结果块按 anthropic
// 语义单独立 user 消息承载。
func TestDecodeRequestWebSearchHistory(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"web_search_call","id":"ws_1","status":"completed","action":{` +
		`"type":"search","query":"weather in Paris","sources":[{"type":"url","url":"https://example.com/p"}]}},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"sunny"}]}]}`)
	req, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var call *ir.ServerToolUse
	var result *ir.WebSearchToolResult
	var resultRole ir.Role
	for _, m := range req.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockServerToolUse:
				call = b.ServerToolUse
			case ir.BlockWebSearchToolResult:
				result = b.WebSearchToolResult
				resultRole = m.Role
			}
		}
	}
	if call == nil || call.ID != "ws_1" || !strings.Contains(string(call.Input), "weather in Paris") {
		t.Errorf("请求历史丢了托管调用：%+v", req.Messages)
	}
	if result == nil || result.ToolUseID != "ws_1" || len(result.Results) != 1 {
		t.Errorf("请求历史丢了搜索结果：%+v", req.Messages)
	}
	if resultRole != ir.RoleUser {
		t.Errorf("结果块应落 user 回合（anthropic 语义），实落 %q", resultRole)
	}
}

// 跨族往返：anthropic 形态的服务端工具历史 -> responses 请求 -> 解回 IR，
// 调用 id、查询串与来源 URL 全程不丢。这条链路是「模型看得见自己上一轮
// 让网关搜了什么」的完整闭环。
func TestWebSearchHistoryRoundTrip(t *testing.T) {
	in := &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
					ID: "srvtoolu_1", Name: "web_search", Input: json.RawMessage(`{"query":"weather in Paris"}`),
				}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{
				{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{
					ToolUseID: "srvtoolu_1",
					Results:   []ir.WebSearchResult{{URL: "https://example.com/p"}},
				}},
			}},
		},
	}
	body, err := New().EncodeRequest(in)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	for _, want := range []string{`"type":"web_search_call"`, "srvtoolu_1", "weather in Paris", "example.com/p"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("出站请求丢了 %q：%s", want, body)
		}
	}
	back, err := New().DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	var call *ir.ServerToolUse
	var result *ir.WebSearchToolResult
	for _, m := range back.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockServerToolUse:
				call = b.ServerToolUse
			case ir.BlockWebSearchToolResult:
				result = b.WebSearchToolResult
			}
		}
	}
	if call == nil || call.ID != "srvtoolu_1" || !strings.Contains(string(call.Input), "weather in Paris") {
		t.Errorf("往返后调用块变形：%+v", back.Messages)
	}
	if result == nil || result.ToolUseID != "srvtoolu_1" || len(result.Results) != 1 ||
		result.Results[0].URL != "https://example.com/p" {
		t.Errorf("往返后结果块变形：%+v", back.Messages)
	}
}

// action <-> server_tool_use input 的互逆变换：四种动作形态各自还原。
// 键位判型（pattern=>find_in_page、url=>open_page、queries/query=>search）
// 判错一档，往返就把动作类型改了。
func TestWSActionRoundTripShapes(t *testing.T) {
	for _, input := range []string{
		`{"query":"q"}`,
		`{"queries":["a","b"]}`,
		`{"url":"https://example.com"}`,
		`{"url":"https://example.com","pattern":"p"}`,
	} {
		a := wsActionFromInput(input, nil)
		back := wsInputFromAction(a)
		var want, got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(input), &want); err != nil {
			t.Fatalf("夹具非法：%v", err)
		}
		if err := json.Unmarshal(back, &got); err != nil {
			t.Fatalf("还原产物非法 JSON：%s（%v）", back, err)
		}
		if len(got) != len(want) {
			t.Errorf("input %s 往返后键位变了：%s", input, back)
			continue
		}
		for k := range want {
			if string(got[k]) != string(want[k]) {
				t.Errorf("input %s 往返后键 %q 变了：%s", input, k, back)
			}
		}
	}
	// 来源列表随 action 往返：results -> sources{url} -> results。
	a := wsActionFromInput(`{"query":"q"}`, []ir.WebSearchResult{{URL: "https://example.com/p"}})
	rs := wsResultsFromAction(a)
	if len(rs) != 1 || rs[0].URL != "https://example.com/p" {
		t.Errorf("来源列表往返丢失：%+v", rs)
	}
}
