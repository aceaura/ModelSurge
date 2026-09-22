// stream_decoder_test.go 上游 OpenAI 流解码的 tool 边界形态回归。
package openaichat

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 首帧同含 id+name+arguments（GLM/智谱形态）时该帧参数不得丢失。
func TestDecodeStreamToolCallSameFrameArgs(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	evs2, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var args string
	for _, ev := range append(evs, evs2...) {
		if ev.Type == ir.EvToolInput {
			args += ev.Text
		}
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("tool arguments = %q, want %q", args, `{"city":"北京"}`)
	}
}

// 参数先于 id/name 到达（先缓存后冲刷）的既有形态不回归。
func TestDecodeStreamToolCallPendingArgs(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	evs2, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"\"北京\"}"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var args string
	for _, ev := range append(evs, evs2...) {
		if ev.Type == ir.EvToolInput {
			args += ev.Text
		}
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("tool arguments = %q, want %q", args, `{"city":"北京"}`)
	}
}

func TestDecodeStreamFallsBackToSmallestChoiceIndex(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	events, err := dec.Feed("data", `{"choices":[{"index":4,"delta":{"content":"D"}},{"index":2,"delta":{"content":"C"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, ev := range events {
		if ev.Type == ir.EvTextDelta {
			text += ev.Text
		}
	}
	if text != "C" {
		t.Fatalf("text = %q, want smallest choice index C", text)
	}
}

func TestDecodeStreamKeepsOneChoice(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	events, err := dec.Feed("data", `{"id":"c1","choices":[{"index":1,"delta":{"content":"B","tool_calls":[{"index":0,"id":"wrong","function":{"name":"wrong","arguments":"{}"}}]},"finish_reason":"length"},{"index":0,"delta":{"content":"A"},"finish_reason":"stop"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, dec.Finish()...)
	var text string
	var stop ir.StopReason
	for _, ev := range events {
		switch ev.Type {
		case ir.EvTextDelta:
			text += ev.Text
		case ir.EvBlockStart:
			if ev.Block != nil && ev.Block.Type == ir.BlockToolUse {
				t.Fatalf("discarded choice leaked tool call: %+v", ev.Block.ToolUse)
			}
		case ir.EvMessageDelta:
			stop = ev.StopReason
		}
	}
	if text != "A" {
		t.Fatalf("text = %q, want A", text)
	}
	if stop != ir.StopEndTurn {
		t.Fatalf("stop = %q, want %q", stop, ir.StopEndTurn)
	}
	reporter := dec.(interface{ Notes() []string })
	notes := reporter.Notes()
	if len(notes) != 1 || notes[0] == "" {
		t.Fatalf("discard note = %v", notes)
	}
	if again := reporter.Notes(); len(again) != 0 {
		t.Fatalf("notes did not drain: %v", again)
	}
}
