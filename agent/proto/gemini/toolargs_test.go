package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 流式编码的截断参数：args 槽是 RawMessage，原文不规整会让这个 chunk
// marshal 失败、整块 functionCall 丢失。两条出口（block_stop 与 Finish
// 冲刷）都要保原文。
func TestStreamEncodeTruncatedToolArgsKept(t *testing.T) {
	build := func() (enc interface {
		Encode(ir.Event) ([][]byte, error)
		Finish() [][]byte
	}) {
		return codec{}.NewStreamEncoder()
	}
	run := func(finishEarly bool) string {
		enc := build()
		evs := []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
			{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f"}}},
			{Type: ir.EvToolInput, Index: 0, Text: `{"city": "Par`},
		}
		var out []byte
		if !finishEarly {
			evs = append(evs, ir.Event{Type: ir.EvBlockStop, Index: 0})
		}
		for _, ev := range evs {
			frames, err := enc.Encode(ev)
			if err != nil {
				t.Fatal(err)
			}
			for _, fr := range frames {
				out = append(out, fr...)
			}
		}
		for _, fr := range enc.Finish() {
			out = append(out, fr...)
		}
		return string(out)
	}
	for _, finishEarly := range []bool{false, true} {
		s := run(finishEarly)
		if !strings.Contains(s, ir.RawArgsKey) {
			t.Errorf("finishEarly=%v 截断原文没挪进 %s: %s", finishEarly, ir.RawArgsKey, s)
		}
		if !strings.Contains(s, "city") {
			t.Errorf("finishEarly=%v 原文片段丢了: %s", finishEarly, s)
		}
	}
}
