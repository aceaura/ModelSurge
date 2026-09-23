package proto_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// 服务端工具块（上游自己执行的搜索）在 OpenAI 两系里都没有对应形态。
// 它们必须整块消失，而不是退化成正文：查询 JSON 拼进 output_text /
// content 会让客户端在回答里看到一段凭空出现的参数串。

const searchQuery = `{"query":"weather in Paris"}`

// serverToolStream 服务端工具的事件形态：
// server_tool_use(+input delta) / web_search_tool_result / 摘要文本。
func serverToolStream() []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type: ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{
				ID: "srvtoolu_1", Name: "web_search", Input: json.RawMessage(searchQuery),
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
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
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
// 流事件被跳过但块仍登记在 order 里时，参数串会从这里漏出去。
func TestServerToolBlocksAbsentFromResponsesFullOutput(t *testing.T) {
	out := streamOut(t, "openai-responses", serverToolStream())
	idx := strings.LastIndex(out, `"type":"response.completed"`)
	if idx < 0 {
		t.Fatalf("没有 response.completed：\n%s", out)
	}
	if final := out[idx:]; strings.Contains(final, "weather in Paris") {
		t.Errorf("全量 output 里泄漏查询参数：\n%s", final)
	}
}

// 被跳过的块不得留下空壳 item：退化成 text 分支时，没有 delta 的结果块会
// 变成一个 output_text 为空串的 message，客户端会多看到一条空回复。
func TestServerToolBlocksLeaveNoEmptyItem(t *testing.T) {
	out := streamOut(t, "openai-responses", serverToolStream())
	if n := strings.Count(out, `"type":"response.output_item.added"`); n != 1 {
		t.Errorf("output_item.added 应只有摘要正文一条，实得 %d：\n%s", n, out)
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

// 非流式方向同样不得泄漏（三个协议都不输出对应形态，这里把行为钉住；
// 「丢了要报出来」由下面的注记测试负责）。
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
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
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
}

// 只有服务端工具块、没有正文时也不能崩，且不得留下空 item。
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
	for _, name := range []string{"openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			out := streamOut(t, name, events)
			if strings.Contains(out, "weather in Paris") {
				t.Errorf("查询参数泄漏：\n%s", out)
			}
		})
	}
}

// 上游断流（没有 block_stop / message_delta）时 Finish 兜底也不能把
// 被跳过的块补出来。
func TestServerToolBlocksAbsentAfterTruncatedStream(t *testing.T) {
	truncated := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{
			Type:          ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search"},
		}},
		{Type: ir.EvToolInput, Index: 0, Text: searchQuery},
	}
	out := streamOut(t, "openai-responses", truncated)
	if strings.Contains(out, "weather in Paris") || strings.Contains(out, "srvtoolu_1") {
		t.Errorf("Finish 兜底把被跳过的块补了出来：\n%s", out)
	}
}

// ---- R95：不泄漏只是一半，丢了还必须报出来 ----
//
// 上面那批测试钉的是「块体不进正文」。但三个外族在流式、非流式与请求侧
// 三条路径上都曾经零注记：调用方拿到的响应看不出这一轮少了什么，模型也
// 不知道自己上一轮搜过什么。以下把可见性钉住。

// serverToolOnly 只有托管工具块（2 个调用 + 1 个结果）。计数刻意不对称，
// 好让「两个计数接反」这类变异被杀。
func serverToolOnly() []ir.Block {
	return []ir.Block{
		{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
			ID: "srvtoolu_1", Name: "web_search", Input: json.RawMessage(searchQuery),
		}},
		{Type: ir.BlockServerToolUse, ServerToolUse: &ir.ServerToolUse{
			ID: "srvtoolu_2", Name: "web_search", Input: json.RawMessage(searchQuery),
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
