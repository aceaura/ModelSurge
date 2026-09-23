package proto_test

import (
	"encoding/json"
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
	return sb.String()
}

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

// 非流式方向同样不得泄漏（三个协议都只是静默丢弃，这里把行为钉住）。
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
