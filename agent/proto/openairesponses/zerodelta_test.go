package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R103-6 零增量工具块的 arguments.done 帧：空串不是合法 JSON，且与同帧
// output_item.done 内 doneItem 归一出的 "{}" 自相矛盾——客户端按哪个帧取
// 参数决定它拿到哪种值。两帧必须同口径归一为 "{}"。
func TestStreamEncodeZeroDeltaToolArgsDoneConsistent(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	var out []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "call_1", Name: "noop"}}},
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
	if !strings.Contains(s, `"response.function_call_arguments.done"`) {
		t.Fatalf("缺 arguments.done 帧：\n%s", s)
	}
	done := s[strings.Index(s, `"response.function_call_arguments.done"`):]
	if !strings.Contains(done, `"arguments":"{}"`) {
		t.Errorf("零增量工具块 done 帧参数没归一为 {}：\n%s", done)
	}
	if strings.Contains(done, `"arguments":""`) {
		t.Errorf("done 帧与 output_item.done 自相矛盾（空串 vs {}）：\n%s", s)
	}
}
