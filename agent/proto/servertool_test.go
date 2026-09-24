package proto_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// 服务端工具块（上游自己执行的搜索）在 openai-chat / gemini 里没有对应形态，
// 必须整块消失，而不是退化成正文：查询 JSON 拼进 output_text / content 会让
// 客户端在回答里看到一段凭空出现的参数串。openai-responses 族有 web_search_call
// 形态：web_search 调用对与结果映射成 item，其余托管工具（web_fetch 等）仍丢弃。

const searchQuery = `{"query":"weather in Paris"}`

// serverToolStreamNamed 服务端工具的事件形态：
// server_tool_use(+input delta) / web_search_tool_result / 摘要文本。
// tool 参数化：openai-responses 族只映射 web_search，其余托管工具仍整块丢弃，
// 两类断言需要不同夹具。
func serverToolStreamNamed(tool string) []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type: ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_1", Name: tool, Input: json.RawMessage(searchQuery),
			},
		}},
		{Type: ir.EvToolInput, Index: 0, Text: searchQuery},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{
			Type: ir.BlockWebSearchToolResult,
			WebSearchToolResult: &ir.WebSearchToolResult{
				ToolUseID: "srvtoolu_1",
				Results:   []ir.WebSearchResult{{Title: "Paris weather", URL: "https://example.com/p"}},
			},
		}},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 2, Text: "It is sunny."},
		{Type: ir.EvBlockStop, Index: 2},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
}

func serverToolStream() []ir.Event { return serverToolStreamNamed("web_search") }

func streamOut(t *testing.T, name string, events []ir.Event) string {
	t.Helper()
	out, _ := streamOutNotes(t, name, events)
	return out
}

// streamOutNotes 同 streamOut，另返回编码器排干的损耗注记。
func streamOutNotes(t *testing.T, name string, events []ir.Event) (string, []string) {
	t.Helper()
	enc := proto.MustInbound(name).NewStreamEncoder()
	var sb strings.Builder
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("%s Encode(%s idx=%d): %v", name, ev.Type, ev.Index, err)
		}
		for _, fr := range frames {
			sb.Write(fr)
		}
	}
	for _, fr := range enc.Finish() {
		sb.Write(fr)
	}
	return sb.String(), enc.Notes()
}

// foreignInbound 入站三外族（anthropic 是这两种块的原生形态）。按纪律硬编码，
// 不动态取协议清单：新协议入列时必须被这里的断言逼着表态。
var foreignInbound = []string{"openai-chat", "openai-responses", "gemini"}

func TestServerToolBlocksNotLeakedIntoStream(t *testing.T) {
	for _, name := range []string{"openai-chat", "gemini"} {
		t.Run(name, func(t *testing.T) {
			out := streamOut(t, name, serverToolStream())
			if strings.Contains(out, "weather in Paris") {
				t.Errorf("服务端工具查询参数泄漏进正文：\n%s", out)
			}
			if strings.Contains(out, "srvtoolu_1") {
				t.Errorf("服务端工具 id 泄漏：\n%s", out)
			}
			if !strings.Contains(out, "It is sunny.") {
				t.Errorf("摘要正文被误删：\n%s", out)
			}
		})
	}
}

// openai-responses 族有 web_search_call 槽位：web_search 调用对不再丢弃，
// 而是映射成托管 item——added 帧开张、结果块到达时补 done 帧（带 query 与来源）。
func TestServerToolBlocksMappedIntoResponsesStream(t *testing.T) {
	out := streamOut(t, "openai-responses", serverToolStream())
	for _, want := range []string{`"type":"web_search_call"`, "srvtoolu_1", "weather in Paris", "example.com/p"} {
		if !strings.Contains(out, want) {
			t.Errorf("responses 流丢了已映射的托管内容 %q：\n%s", want, out)
		}
	}
	if !strings.Contains(out, "It is sunny.") {
		t.Errorf("摘要正文被误删：\n%s", out)
	}
	if strings.Contains(out, "weather in Paris") &&
		!strings.Contains(out, `"type":"web_search_call"`) {
		t.Errorf("查询参数以非 item 形态泄漏：\n%s", out)
	}
	// 非 web_search 的托管工具仍无槽位，必须整块消失。
	out2 := streamOut(t, "openai-responses", serverToolStreamNamed("web_fetch"))
	if strings.Contains(out2, "weather in Paris") || strings.Contains(out2, "srvtoolu_1") {
		t.Errorf("未映射的托管工具块泄漏：\n%s", out2)
	}
}

// Anthropic 是这两种块的原生形态，必须原样保留——
// 「不泄漏」的规则只适用于装不下它们的协议。
func TestServerToolBlocksPreservedForAnthropic(t *testing.T) {
	out := streamOut(t, "anthropic", serverToolStream())
	for _, want := range []string{"server_tool_use", "web_search_tool_result", "srvtoolu_1"} {
		if !strings.Contains(out, want) {
			t.Errorf("anthropic 丢了原生块 %q：\n%s", want, out)
		}
	}
}

// Responses 的 response.completed 带全量 output（SDK get_final_response 依赖）。
// 已映射的 web_search 对必须出现在这里，与流帧一致；被丢弃的块（web_fetch）
// 在流事件里被跳过后也不得从这条后路漏出去。
func TestServerToolBlocksInResponsesFullOutput(t *testing.T) {
	out := streamOut(t, "openai-responses", serverToolStream())
	idx := strings.LastIndex(out, `"type":"response.completed"`)
	if idx < 0 {
		t.Fatalf("没有 response.completed：\n%s", out)
	}
	final := out[idx:]
	for _, want := range []string{`"type":"web_search_call"`, "weather in Paris", "example.com/p", `"status":"completed"`} {
		if !strings.Contains(final, want) {
			t.Errorf("全量 output 丢了已映射的托管内容 %q：\n%s", want, final)
		}
	}
	out2 := streamOut(t, "openai-responses", serverToolStreamNamed("web_fetch"))
	idx2 := strings.LastIndex(out2, `"type":"response.completed"`)
	if idx2 < 0 {
		t.Fatalf("没有 response.completed：\n%s", out2)
	}
	if final := out2[idx2:]; strings.Contains(final, "weather in Paris") {
		t.Errorf("全量 output 里泄漏未映射工具的查询参数：\n%s", final)
	}
}

// 被映射/被跳过的块都不得留下空壳 item：映射后 added 帧应是 web_search_call +
// 摘要 message 共两条；没有 delta 的结果块不许变成空 output_text 的 message。
func TestServerToolBlocksLeaveNoEmptyItem(t *testing.T) {
	out := streamOut(t, "openai-responses", serverToolStream())
	if n := strings.Count(out, `"type":"response.output_item.added"`); n != 2 {
		t.Errorf("output_item.added 应为 web_search_call+摘要两条，实得 %d：\n%s", n, out)
	}
	if strings.Contains(out, `"type":"output_text","text":""`) {
		t.Errorf("留下了空 output_text 壳：\n%s", out)
	}
	idx := strings.LastIndex(out, `"type":"response.completed"`)
	if idx < 0 {
		t.Fatalf("没有 response.completed：\n%s", out)
	}
	if n := strings.Count(out[idx:], `"type":"message"`); n != 1 {
		t.Errorf("全量 output 应只有一条 message，实得 %d：\n%s", n, out[idx:])
	}
}

// 跳过服务端工具块不能顺手把普通工具调用的参数一起吞掉：
// 二者都走 EvToolInput，只有前者在 skip 集合里。
func TestRegularToolArgumentsStillReachWire(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type:    ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "get_weather"},
		}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"city":"Paris"}`},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse},
		{Type: ir.EvMessageStop},
	}
	for _, name := range []string{"openai-responses", "openai-chat"} {
		t.Run(name, func(t *testing.T) {
			out := streamOut(t, name, events)
			if !strings.Contains(out, `Paris`) {
				t.Errorf("普通工具调用参数被误吞：\n%s", out)
			}
			if !strings.Contains(out, "get_weather") {
				t.Errorf("普通工具名丢失：\n%s", out)
			}
		})
	}
}

// 非流式方向：openai-chat / gemini 不输出对应形态，不得泄漏；openai-responses
// 把 web_search 对编码成 web_search_call item，查询与结果必须保留。
func TestServerToolBlocksNotLeakedIntoResponse(t *testing.T) {
	resp := &ir.Response{
		ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_1", Name: "web_search", Input: json.RawMessage(searchQuery),
			}},
			{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "srvtoolu_1"}},
			{Type: ir.BlockText, Text: "It is sunny."},
		},
	}
	for _, name := range []string{"openai-chat", "gemini"} {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustInbound(name).EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if strings.Contains(string(body), "weather in Paris") || strings.Contains(string(body), "srvtoolu_1") {
				t.Errorf("非流式响应泄漏服务端工具块：%s", body)
			}
			if !strings.Contains(string(body), "It is sunny.") {
				t.Errorf("摘要正文被误删：%s", body)
			}
		})
	}
	t.Run("openai-responses", func(t *testing.T) {
		body, err := proto.MustInbound("openai-responses").EncodeResponse(resp)
		if err != nil {
			t.Fatalf("EncodeResponse: %v", err)
		}
		for _, want := range []string{`"type":"web_search_call"`, "srvtoolu_1", "weather in Paris", "It is sunny."} {
			if !strings.Contains(string(body), want) {
				t.Errorf("非流式响应丢了已映射的托管内容 %q：%s", want, body)
			}
		}
	})
}

// 只有服务端工具块、没有正文时也不能崩，且不得留下空 item。
// openai-chat 无槽位整块丢弃；openai-responses 映射成 web_search_call。
func TestServerToolOnlyStreamDoesNotFail(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type:          ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search"},
		}},
		{Type: ir.EvToolInput, Index: 0, Text: searchQuery},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	}
	out := streamOut(t, "openai-chat", events)
	if strings.Contains(out, "weather in Paris") {
		t.Errorf("查询参数泄漏：\n%s", out)
	}
	out = streamOut(t, "openai-responses", events)
	for _, want := range []string{`"type":"web_search_call"`, "srvtoolu_1", "weather in Paris"} {
		if !strings.Contains(out, want) {
			t.Errorf("responses 流丢了已映射的托管内容 %q：\n%s", want, out)
		}
	}
}

// 上游断流（没有 block_stop / message_delta）时 Finish 兜底：已映射的
// web_search 调用要补出 done 帧（无结果来源），而不是连调用一起消失。
func TestServerToolMappedAfterTruncatedStream(t *testing.T) {
	truncated := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type:          ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search"},
		}},
		{Type: ir.EvToolInput, Index: 0, Text: searchQuery},
	}
	out := streamOut(t, "openai-responses", truncated)
	for _, want := range []string{`"type":"web_search_call"`, "srvtoolu_1", "weather in Paris"} {
		if !strings.Contains(out, want) {
			t.Errorf("Finish 兜底丢了已映射的托管调用 %q：\n%s", want, out)
		}
	}
	// 未映射的托管工具（web_fetch）断流时仍整块消失。
	truncated[1].Block.ServerToolUse.Name = "web_fetch"
	out = streamOut(t, "openai-responses", truncated)
	if strings.Contains(out, "weather in Paris") || strings.Contains(out, "srvtoolu_1") {
		t.Errorf("Finish 兜底把未映射的被跳过块补了出来：\n%s", out)
	}
}

// ---- R95：不泄漏只是一半，丢了还必须报出来 ----
//
// 上面那批测试钉的是「块体不进正文」。但三个外族在流式、非流式与请求侧
// 三条路径上都曾经零注记：调用方拿到的响应看不出这一轮少了什么，模型也
// 不知道自己上一轮搜过什么。以下把可见性钉住。

// serverToolOnly 只有托管工具块（2 个调用 + 1 个结果）。计数刻意不对称，
// 好让「两个计数接反」这类变异被杀。工具名用 web_fetch：web_search 在
// openai-responses 族已映射成 web_search_call（不再算损耗），web_fetch
// 在三外族都没有槽位，仍是纯丢弃路径。
func serverToolOnly() []ir.Block {
	return []ir.Block{
		{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
			ID: "srvtoolu_1", Name: "web_fetch", Input: json.RawMessage(searchQuery),
		}},
		{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
			ID: "srvtoolu_2", Name: "web_fetch", Input: json.RawMessage(searchQuery),
		}},
		{Type: ir.BlockWebSearchToolResult, WebSearchToolResult: &ir.WebSearchToolResult{
			ToolUseID: "srvtoolu_1",
			Results:   []ir.WebSearchResult{{Title: "Paris weather", URL: "https://example.com/p", Snippet: "sunny"}},
		}},
	}
}

// serverToolResp 托管工具块夹在正文中间：既报损耗，也不许动正文。
func serverToolResp() *ir.Response {
	blocks := append([]ir.Block{{Type: ir.BlockText, Text: "lead"}}, serverToolOnly()...)
	blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: "It is sunny."})
	return &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn, Content: blocks}
}

// blockEvents 把块序列摊成最小事件流（每块只有 start/stop，无 delta）。
func blockEvents(blocks []ir.Block) []ir.Event {
	evs := []ir.Event{{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"}}
	for i := range blocks {
		b := blocks[i]
		evs = append(evs,
			ir.Event{Type: ir.EvBlockStart, Index: i, Block: &b},
			ir.Event{Type: ir.EvBlockStop, Index: i})
	}
	return append(evs,
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		ir.Event{Type: ir.EvMessageStop})
}

// assertNoSessionContent 注记里不得出现搜索结果的标题/URL/摘要或查询参数：
// 那是会话内容，注记会进响应头与日志。
func assertNoSessionContent(t *testing.T, notes []string) {
	t.Helper()
	for _, n := range notes {
		for _, leak := range []string{"example.com", "Paris weather", "sunny", "weather in Paris"} {
			if strings.Contains(n, leak) {
				t.Errorf("注记带出了会话内容 %q：%s", leak, n)
			}
		}
	}
}

func TestServerToolDropReportedInStreamNotes(t *testing.T) {
	if proto.ServerToolDropNote(2, 1) == proto.ServerToolDropNote(1, 2) {
		t.Fatal("注记措辞对两个计数对称，测不出计数接反")
	}
	want := proto.ServerToolDropNote(2, 1)
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			_, notes := streamOutNotes(t, name, blockEvents(serverToolOnly()))
			if !slices.Contains(notes, want) {
				t.Fatalf("流式未报托管工具块损耗：notes=%q", notes)
			}
			assertNoSessionContent(t, notes)
		})
	}
}

func TestServerToolDropReportedInResponseNotes(t *testing.T) {
	want := proto.ServerToolDropNote(2, 1)
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			notes := proto.MustInbound(name).ResponseNotes(serverToolResp())
			if !slices.Contains(notes, want) {
				t.Fatalf("非流式扫描未报托管工具块损耗：notes=%q", notes)
			}
			assertNoSessionContent(t, notes)
			body, err := proto.MustInbound(name).EncodeResponse(serverToolResp())
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			for _, keep := range []string{"lead", "It is sunny."} {
				if !strings.Contains(string(body), keep) {
					t.Errorf("报了损耗却顺手删了正文 %q：%s", keep, body)
				}
			}
		})
	}
}

// 只有调用没有结果（上游断在两者之间）时也得报，且计数只带非零那半。
func TestServerToolDropReportedForCallsOnly(t *testing.T) {
	blocks := serverToolOnly()[:2]
	want := proto.ServerToolDropNote(2, 0)
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			_, notes := streamOutNotes(t, name, blockEvents(blocks))
			if !slices.Contains(notes, want) {
				t.Fatalf("只有托管调用时未报损耗：notes=%q", notes)
			}
			scan := proto.MustInbound(name).ResponseNotes(&ir.Response{
				ID: "m1", Model: "m", StopReason: ir.StopEndTurn, Content: blocks})
			if !slices.Contains(scan, want) {
				t.Fatalf("只有托管调用时非流式扫描未报损耗：notes=%q", scan)
			}
		})
	}
}

// anthropic 是原生形态：块要照常输出，注记必须闭嘴——否则等于谎报损耗。
func TestServerToolDropSilentForAnthropic(t *testing.T) {
	for _, notes := range [][]string{
		proto.MustInbound("anthropic").ResponseNotes(serverToolResp()),
		streamAnthropicNotes(t),
	} {
		for _, n := range notes {
			if strings.Contains(n, "server-side tool") || strings.Contains(n, "web search result") {
				t.Errorf("anthropic 谎报托管工具块损耗：%s", n)
			}
		}
	}
}

func streamAnthropicNotes(t *testing.T) []string {
	t.Helper()
	_, notes := streamOutNotes(t, "anthropic", blockEvents(serverToolOnly()))
	return notes
}

// 注记主语按计数分三种形态；两种块型都为零时不该被调用方拿来生成注记，
// 这里只钉措辞本身。
func TestServerToolDropNoteSubjectShapes(t *testing.T) {
	for _, tc := range []struct {
		calls, results int
		prefix         string
	}{
		{1, 0, "dropped 1 server-side tool call(s):"},
		{0, 1, "dropped 1 web search result block(s):"},
		{2, 3, "dropped 2 server-side tool call(s) and 3 web search result block(s):"},
	} {
		got := proto.ServerToolDropNote(tc.calls, tc.results)
		if !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("ServerToolDropNote(%d,%d) 主语不对：\n got %s\nwant 前缀 %s",
				tc.calls, tc.results, got, tc.prefix)
		}
	}
}

// Notes() 排干后必须归零：relay 在流尾与聚合两处都会取，重复报等于谎报两次。
func TestServerToolDropNotesDrained(t *testing.T) {
	enc := proto.MustInbound("openai-responses").NewStreamEncoder()
	for _, ev := range blockEvents(serverToolOnly()) {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	enc.Finish()
	if first := enc.Notes(); !slices.Contains(first, proto.ServerToolDropNote(2, 1)) {
		t.Fatalf("首次 Notes() 没有损耗注记：%q", first)
	}
	if again := enc.Notes(); slices.Contains(again, proto.ServerToolDropNote(2, 1)) {
		t.Errorf("Notes() 未排干，重复报出：%q", again)
	}
}
