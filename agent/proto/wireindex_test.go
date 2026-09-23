package proto_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// Responses 的 output_index 是客户端索引 response.output[] 的下标（官方 SDK 的
// 流式累加器正是这么用的）。它必须与 response.completed 的全量 output 数组
// 稠密对齐：被跳过的块（服务端工具、container_upload、涂抹思考、不透明块）
// 在数组里没有条目，若仍占掉一个序号，后续块的 output_index 就会越过数组末尾。

// assertDenseOutputIndex 断言帧里出现过的 output_index 恰好是 0..len(output)-1，
// 且首次出现顺序递增——既查越界，也查错位（错位等于把 delta 挂到别的 item 上）。
func assertDenseOutputIndex(t *testing.T, out string) []int {
	t.Helper()
	var seen []int
	completed := -1
	for _, frame := range strings.Split(out, "\n\n") {
		frame = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(frame), "data:"))
		if frame == "" || frame == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(frame), &ev); err != nil {
			t.Fatalf("帧不是合法 JSON：%q（%v）", frame, err)
		}
		if v, ok := ev["output_index"].(float64); ok {
			i := int(v)
			if !slices.Contains(seen, i) {
				seen = append(seen, i)
			}
		}
		typ, _ := ev["type"].(string)
		if typ == "response.completed" || typ == "response.incomplete" {
			resp, _ := ev["response"].(map[string]any)
			arr, _ := resp["output"].([]any)
			completed = len(arr)
		}
	}
	if completed < 0 {
		t.Fatalf("没有终止帧，无法核对全量 output：\n%s", out)
	}
	for n, i := range seen {
		if i != n {
			t.Fatalf("output_index 与全量 output 错位：第 %d 个开块用的是 %d，应是 %d\n%s", n, i, n, out)
		}
	}
	if len(seen) != completed {
		t.Fatalf("帧里出现 %d 个 output_index，全量 output 却有 %d 条\n%s", len(seen), completed, out)
	}
	return seen
}

// 被跳过的服务端工具块不得烧掉序号：此前唯一的正文块拿到 output_index=2，
// 而全量 output 只有 1 条。
func TestSkippedBlocksDoNotBurnOutputIndex(t *testing.T) {
	seen := assertDenseOutputIndex(t, streamOut(t, "openai-responses", serverToolStream()))
	if len(seen) != 1 {
		t.Errorf("应只开出一个 item，实得 %d：%v", len(seen), seen)
	}
}

// 跳过发生在中段时同样要对齐：前一个正文块占了 0，后一个必须紧接 1。
func TestOutputIndexStaysDenseAcrossMidStreamSkips(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "first"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockRedactedThinking, RedactedData: "CIPHER"}},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockServerToolUse,
			ServerToolUse: &ir.ServerToolUse{ID: "srvtoolu_1", Name: "web_search"}}},
		{Type: ir.EvBlockStop, Index: 2},
		// 四类被跳过的块型都要在夹具里出现一次：漏掉哪一类，那一类的「跳过前
		// 先烧序号」变异就没有夹具能走到，会存活。
		{Type: ir.EvBlockStart, Index: 3, Block: &ir.Block{Type: ir.BlockContainerUpload,
			ContainerUpload: &ir.ContainerUploadRef{FileID: "file_1"}}},
		{Type: ir.EvBlockStop, Index: 3},
		{Type: ir.EvBlockStart, Index: 4, Block: &ir.Block{Type: ir.BlockOpaque,
			Opaque: &ir.Opaque{WireType: "web_fetch_tool_result", Body: json.RawMessage(`{"u":"x"}`), From: "anthropic"}}},
		{Type: ir.EvBlockStop, Index: 4},
		{Type: ir.EvBlockStart, Index: 5, Block: &ir.Block{Type: ir.BlockWebSearchToolResult,
			WebSearchToolResult: &ir.WebSearchToolResult{ToolUseID: "srvtoolu_1"}}},
		{Type: ir.EvBlockStop, Index: 5},
		{Type: ir.EvBlockStart, Index: 6, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 6, Text: "second"},
		{Type: ir.EvBlockStop, Index: 6},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 2}},
		{Type: ir.EvMessageStop},
	}
	out := streamOut(t, "openai-responses", events)
	seen := assertDenseOutputIndex(t, out)
	if len(seen) != 2 {
		t.Errorf("应开出两个 item，实得 %d：%v", len(seen), seen)
	}
	// delta 必须挂在各自的 item 上，错位会让两段正文互相覆盖。
	if !strings.Contains(out, `"output_index":0,"content_index":0,"delta":"first"`) {
		t.Errorf("first 未挂在 output_index 0：\n%s", out)
	}
	if !strings.Contains(out, `"output_index":1,"content_index":0,"delta":"second"`) {
		t.Errorf("second 未挂在 output_index 1：\n%s", out)
	}
}

// 工具调用块也要参与稠密编号：它有自己的 output_index，跳过的块夹在中间时
// 同样不得让它越界。
func TestToolCallOutputIndexSurvivesSkips(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockOpaque,
			Opaque: &ir.Opaque{WireType: "web_fetch_tool_result", Body: json.RawMessage(`{"u":"x"}`), From: "anthropic"}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "t1", Name: "lookup", Kind: ir.ToolFunction}}},
		{Type: ir.EvToolInput, Index: 1, Text: `{"q":"a"}`},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse, Usage: &ir.Usage{OutputTokens: 1}},
		{Type: ir.EvMessageStop},
	}
	out := streamOut(t, "openai-responses", events)
	seen := assertDenseOutputIndex(t, out)
	if len(seen) != 1 {
		t.Errorf("应只开出工具调用一个 item，实得 %d：%v", len(seen), seen)
	}
	if !strings.Contains(out, `"type":"response.function_call_arguments.done","output_index":0`) {
		t.Errorf("工具参数终止帧的 output_index 不是 0：\n%s", out)
	}
}
