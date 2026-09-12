package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"relayd/backend/ir"
)

// fakeSearchCaller webSearchCaller 的测试替身：返回固定结果或错误，
// 记录收到的 query。
type fakeSearchCaller struct {
	err   error
	calls []string
}

func (f *fakeSearchCaller) CallWebSearch(ctx context.Context, query string) (string, []ir.WebSearchResult, error) {
	f.calls = append(f.calls, query)
	if f.err != nil {
		return "", nil, f.err
	}
	return "srvtoolu_fake1", []ir.WebSearchResult{
		{Title: "Go", URL: "https://go.dev", Snippet: "fast"},
	}, nil
}

// kiroFinishLike 构造 kiro 解码器 Finish 形态的事件序列：
// [text 块关闭] [工具块三元组们] [message_delta] [message_stop]。
func kiroFinishLike(textIdx int, tools ...[3]string) []ir.Event {
	var events []ir.Event
	if textIdx >= 0 {
		events = append(events, ir.Event{Type: ir.EvBlockStop, Index: textIdx})
	}
	for i, tc := range tools {
		idx := textIdx + 1 + i
		events = append(events, ir.Event{
			Type: ir.EvBlockStart, Index: idx,
			Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: tc[0], Name: tc[1]}},
		})
		if tc[2] != "" {
			events = append(events, ir.Event{Type: ir.EvToolInput, Index: idx, Text: tc[2]})
		}
		events = append(events, ir.Event{Type: ir.EvBlockStop, Index: idx})
	}
	events = append(events,
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse},
		ir.Event{Type: ir.EvMessageStop})
	return events
}

// hasWebSearchTool：具名与 Hosted 两种声明都命中。
func TestHasWebSearchTool(t *testing.T) {
	req := &ir.Request{}
	if hasWebSearchTool(req) {
		t.Error("no tools")
	}
	req.Tools = []ir.Tool{{Name: "web_search"}}
	if !hasWebSearchTool(req) {
		t.Error("named web_search")
	}
	req.Tools = []ir.Tool{{Hosted: ir.HostedWebSearch}}
	if !hasWebSearchTool(req) {
		t.Error("hosted web_search")
	}
	req.Tools = []ir.Tool{{Name: "read_file"}}
	if hasWebSearchTool(req) {
		t.Error("unrelated tool")
	}
}

// extractQuery：合法 JSON 取 query；残缺/空/无字段返回空。
func TestExtractQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"query":"go release"}`, "go release"},
		{`  {"query":"x"}  `, "x"},
		{`{}`, ""},
		{`{"other":1}`, ""},
		{`{"query":`, ""},
		{``, ""},
	}
	for _, c := range cases {
		if got := extractQuery(c.in); got != c.want {
			t.Errorf("extractQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// formatSearchSummary：<web_search> 包裹、条目编号、空结果提示。
func TestFormatSearchSummary(t *testing.T) {
	s := formatSearchSummary("go", []ir.WebSearchResult{
		{Title: "Go", URL: "https://go.dev", Snippet: "fast"},
		{Title: "Wiki"},
	})
	for _, want := range []string{
		`<web_search>`,
		`Search results for "go"`,
		`1. Title: **Go**`,
		`URL: https://go.dev`,
		`fast`,
		`2. Title: **Wiki**`,
		`</web_search>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
	if s := formatSearchSummary("x", nil); !strings.Contains(s, "No results found") {
		t.Errorf("empty summary = %q", s)
	}
}

// rewriteWebSearchEvents：单 web_search 调用被替换为三块序列，
// 索引从首个工具块起重排，stop_reason 改 end_turn。
func TestRewriteWebSearchEvents_Intercept(t *testing.T) {
	client := &fakeSearchCaller{}
	events := kiroFinishLike(0, [3]string{"toolu_1", "web_search", `{"query":"go"}`})
	out := rewriteWebSearchEvents(context.Background(), client, events)

	if len(client.calls) != 1 || client.calls[0] != "go" {
		t.Fatalf("mcp calls = %v", client.calls)
	}
	if len(out) != 11 { // text stop + 8 合成事件 + message_delta + message_stop
		t.Fatalf("events len = %d, want 11: %+v", len(out), out)
	}
	// 索引重排：server_tool_use=1, result=2, text=3
	stu := out[1]
	if stu.Type != ir.EvBlockStart || stu.Index != 1 || stu.Block.Type != ir.BlockServerToolUse {
		t.Errorf("server_tool_use start = %+v", stu)
	}
	if stu.Block.ServerToolUse.Name != "web_search" || stu.Block.ServerToolUse.ID != "srvtoolu_fake1" {
		t.Errorf("server_tool_use = %+v", stu.Block.ServerToolUse)
	}
	var q map[string]string
	if err := json.Unmarshal(stu.Block.ServerToolUse.Input, &q); err != nil || q["query"] != "go" {
		t.Errorf("input = %s", stu.Block.ServerToolUse.Input)
	}
	if out[2].Type != ir.EvToolInput || out[2].Index != 1 {
		t.Errorf("input delta = %+v", out[2])
	}
	res := out[4]
	if res.Type != ir.EvBlockStart || res.Index != 2 || res.Block.Type != ir.BlockWebSearchToolResult {
		t.Errorf("result start = %+v", res)
	}
	if res.Block.WebSearchToolResult.ToolUseID != "srvtoolu_fake1" || len(res.Block.WebSearchToolResult.Results) != 1 {
		t.Errorf("results = %+v", res.Block.WebSearchToolResult)
	}
	if out[7].Type != ir.EvTextDelta || out[7].Index != 3 || !strings.Contains(out[7].Text, "<web_search>") {
		t.Errorf("summary delta = %+v", out[7])
	}
	// stop_reason：全部拦截 -> end_turn
	var delta *ir.Event
	for i := range out {
		if out[i].Type == ir.EvMessageDelta {
			delta = &out[i]
		}
	}
	if delta == nil || delta.StopReason != ir.StopEndTurn {
		t.Errorf("message_delta = %+v, want end_turn", delta)
	}
}

// 混合工具：web_search 拦截后剩余普通工具保留，索引顺延，
// stop_reason 保持 tool_use。
func TestRewriteWebSearchEvents_Mixed(t *testing.T) {
	client := &fakeSearchCaller{}
	events := kiroFinishLike(0,
		[3]string{"toolu_ws", "web_search", `{"query":"go"}`},
		[3]string{"toolu_r", "read_file", `{"path":"x"}`},
	)
	out := rewriteWebSearchEvents(context.Background(), client, events)

	// 块序：text(0) stop、server_tool_use(1)、result(2)、summary(3)、read_file(4)
	var types []string
	var lastToolUse *ir.Block
	for _, ev := range out {
		if ev.Type == ir.EvBlockStart && ev.Block != nil {
			types = append(types, fmt.Sprintf("%s@%d", ev.Block.Type, ev.Index))
			if ev.Block.Type == ir.BlockToolUse {
				lastToolUse = ev.Block
			}
		}
	}
	want := []string{"server_tool_use@1", "web_search_tool_result@2", "text@3", "tool_use@4"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Errorf("block types = %v, want %v", types, want)
	}
	if lastToolUse == nil || lastToolUse.ToolUse.Name != "read_file" || lastToolUse.ToolUse.ID != "toolu_r" {
		t.Errorf("remaining tool_use = %+v", lastToolUse)
	}
	var delta *ir.Event
	for i := range out {
		if out[i].Type == ir.EvMessageDelta {
			delta = &out[i]
		}
	}
	if delta == nil || delta.StopReason != ir.StopToolUse {
		t.Errorf("stop_reason = %+v, want tool_use", delta)
	}
}

// MCP 失败 / 无 query：降级为普通 tool_use 原样透传。
func TestRewriteWebSearchEvents_Degrade(t *testing.T) {
	client := &fakeSearchCaller{err: fmt.Errorf("mcp down")}
	events := kiroFinishLike(0, [3]string{"toolu_1", "web_search", `{"query":"go"}`})
	out := rewriteWebSearchEvents(context.Background(), client, events)
	if len(out) != len(events) {
		t.Fatalf("degraded events len = %d, want %d", len(out), len(events))
	}
	if out[1].Block.Type != ir.BlockToolUse {
		t.Errorf("degraded first block = %+v, want tool_use passthrough", out[1].Block)
	}

	// 无 query 参数：不调 MCP，原样透传
	client2 := &fakeSearchCaller{}
	events2 := kiroFinishLike(0, [3]string{"toolu_1", "web_search", `{}`})
	out2 := rewriteWebSearchEvents(context.Background(), client2, events2)
	if len(out2) != len(events2) {
		t.Errorf("no-query events len = %d, want %d", len(out2), len(events2))
	}
	if len(client2.calls) != 0 {
		t.Errorf("mcp calls = %v, want none", client2.calls)
	}
}
