package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 同一 index 带新 id 复用槽位：这是两次调用，参数必须分开成两个块。
// 不拆开会把 B 的参数粘到 A 后面，且 B 的 id 被吞掉——客户端看到的
// 是一次参数损坏的调用（new-api 显式处理此形态，sub2api 没处理）。
func TestStreamDecodeToolIndexReuseWithNewID(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	chunks := []string{
		`{"id":"x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_A","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		// 上游复用 index=0 发第二次调用（部分兼容端形态）
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_B","function":{"name":"g","arguments":"{\"b\":2}"}}]}}]}`,
		`{"choices":[{"finish_reason":"tool_calls"}]}`,
	}
	var evs []ir.Event
	for _, c := range chunks {
		out, err := dec.Feed("", c)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, out...)
	}
	evs = append(evs, dec.Finish()...)

	var starts []ir.Event
	var inputs []ir.Event
	for _, ev := range evs {
		switch ev.Type {
		case ir.EvBlockStart:
			if ev.Block != nil && ev.Block.Type == ir.BlockToolUse {
				starts = append(starts, ev)
			}
		case ir.EvToolInput:
			inputs = append(inputs, ev)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("工具块开始数 = %d，两次调用粘成一次: %+v", len(starts), evs)
	}
	if starts[0].Block.ToolUse.ID != "call_A" || starts[1].Block.ToolUse.ID != "call_B" {
		t.Errorf("调用 id = %q / %q", starts[0].Block.ToolUse.ID, starts[1].Block.ToolUse.ID)
	}
	// A 的参数不能混进 B 的块：逐块收集参数片段。
	argsOf := map[int]string{}
	for _, ev := range inputs {
		argsOf[ev.Index] += ev.Text
	}
	if got := argsOf[starts[0].Index]; got != `{"a":1}` {
		t.Errorf("A 参数 = %q, 粘连或丢失", got)
	}
	if got := argsOf[starts[1].Index]; got != `{"b":2}` {
		t.Errorf("B 参数 = %q, 粘连或丢失", got)
	}
	// A 的块必须在 B 开始前关闭，否则下游编码器会把两个调用叠在同一槽位。
	closedBefore := false
	seenStopA := false
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStop && ev.Index == starts[0].Index {
			seenStopA = true
		}
		if ev.Type == ir.EvBlockStart && ev.Index == starts[1].Index && seenStopA {
			closedBefore = true
		}
	}
	if !closedBefore {
		t.Errorf("B 块开始时 A 块尚未关闭: %+v", evs)
	}
}

// 同 id 续传必须不受影响：id 相同只是参数分片，不得误拆。
func TestStreamDecodeSameIDContinuesSameBlock(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	chunks := []string{
		`{"id":"x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_A","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_A","function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"finish_reason":"tool_calls"}]}`,
	}
	var evs []ir.Event
	for _, c := range chunks {
		out, err := dec.Feed("", c)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, out...)
	}
	starts := 0
	args := ""
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block != nil && ev.Block.Type == ir.BlockToolUse {
			starts++
		}
		if ev.Type == ir.EvToolInput {
			args += ev.Text
		}
	}
	if starts != 1 {
		t.Fatalf("同 id 续传被拆成 %d 个块", starts)
	}
	if !strings.Contains(args, `"a":1}`) {
		t.Errorf("参数 = %q", args)
	}
}
