package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R103-7 零增量 tool_use 块的收尾。客户端（含官方 SDK）只从 input_json_delta
// 拼参数，一个 delta 都不发就等于参数是空串——不是合法 JSON。关块前必须补
// 一个 "{}" delta（sub2api 同款：clients assemble tool input exclusively
// from deltas）。
func TestStreamEncodeZeroDeltaToolUseGetsEmptyObjectDelta(t *testing.T) {
	enc := New().NewStreamEncoder()
	var out []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "tu_1", Name: "noop"}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			out = append(out, f...)
		}
	}
	s := string(out)
	if !strings.Contains(s, `"input_json_delta"`) || !strings.Contains(s, `"partial_json":"{}"`) {
		t.Errorf("零增量工具块没补收尾 {} delta：\n%s", s)
	}
	// 补的必须是 "{}" 恰好一次，且不能记畸形参数（空参数归一为合法 {}）。
	if n := strings.Count(s, `"partial_json"`); n != 1 {
		t.Errorf("收尾 delta 应恰好一个，实得 %d：\n%s", n, s)
	}
	if notes := enc.(interface{ Notes() []string }).Notes(); len(notes) != 0 {
		t.Errorf("零增量不该记畸形参数注记：%q", notes)
	}
}

// 对照组：有增量的工具块不补 "{}"——参数只由真实增量组成。
func TestStreamEncodeToolUseWithDeltasNoExtraEmptyObject(t *testing.T) {
	enc := New().NewStreamEncoder()
	var out []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "tu_1", Name: "f"}}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"a":`},
		{Type: ir.EvToolInput, Index: 0, Text: `1}`},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			out = append(out, f...)
		}
	}
	s := string(out)
	if strings.Contains(s, `"partial_json":"{}"`) {
		t.Errorf("有增量的工具块被多补了 {} delta：\n%s", s)
	}
	if n := strings.Count(s, `"input_json_delta"`); n != 2 {
		t.Errorf("参数 delta 应恰好两个，实得 %d：\n%s", n, s)
	}
}
