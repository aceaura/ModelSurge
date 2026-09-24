package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 漏发 output_item.added 的畸形工具流：身份随 output_item.done 到达。
// 块必须以 done 帧的身份开出（id/name 非空），先到的参数碎片回放交付、
// 不重复不丢失。
func TestStreamToolCallMissingAddedIdentityFromDone(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"query\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"weather\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call",`+
			`"call_id":"call_1","name":"get_weather","arguments":"{\"query\":\"weather\"}"}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`)

	var starts []ir.Block
	var args string
	var stops int
	var stopReason ir.StopReason
	for _, ev := range evs {
		switch ev.Type {
		case ir.EvBlockStart:
			starts = append(starts, *ev.Block)
		case ir.EvToolInput:
			args += ev.Text
		case ir.EvBlockStop:
			stops++
		case ir.EvMessageDelta:
			stopReason = ev.StopReason
		}
	}
	if len(starts) != 1 || starts[0].Type != ir.BlockToolUse {
		t.Fatalf("工具块没开或开错：%+v", starts)
	}
	tu := starts[0].ToolUse
	if tu.ID != "call_1" || tu.Name != "get_weather" {
		t.Fatalf("块身份没取 done 帧：%+v", tu)
	}
	if args != `{"query":"weather"}` {
		t.Fatalf("参数碎片没拼全：%q", args)
	}
	if stops != 1 {
		t.Fatalf("工具块没关：%d", stops)
	}
	if stopReason != ir.StopToolUse {
		t.Fatalf("停止原因没判成工具调用：%v", stopReason)
	}
	if n, ok := dec.(interface{ Notes() []string }); ok {
		if notes := n.Notes(); len(notes) != 0 {
			t.Errorf("身份已到不该有注记：%q", notes)
		}
	}
}

// 更畸形的流：只有参数增量，added 与 output_item.done 全缺。terminalEvents
// 合成 id 开块保住参数，Notes() 报出合成数且排干归零。
func TestStreamToolCallNoIdentitySynthID(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":1}"}`,
		`{"type":"response.completed","response":{}}`)

	var tu *ir.ToolUse
	var args string
	var stopReason ir.StopReason
	for _, ev := range evs {
		switch ev.Type {
		case ir.EvBlockStart:
			if ev.Block.Type == ir.BlockToolUse {
				tu = ev.Block.ToolUse
			}
		case ir.EvToolInput:
			args += ev.Text
		case ir.EvMessageDelta:
			stopReason = ev.StopReason
		}
	}
	if tu == nil || tu.ID == "" {
		t.Fatalf("身份全程未到却没合成 id 开块：%+v", evs)
	}
	if args != `{"a":1}` {
		t.Fatalf("参数没保住：%q", args)
	}
	if stopReason != ir.StopToolUse {
		t.Fatalf("停止原因没判成工具调用：%v", stopReason)
	}
	n := dec.(interface{ Notes() []string })
	notes := n.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "1 streamed tool call(s)") {
		t.Fatalf("合成 id 未报注记：%q", notes)
	}
	if strings.Contains(notes[0], `{"a":1}`) {
		t.Errorf("注记带出了工具参数：%s", notes[0])
	}
	if again := n.Notes(); len(again) != 0 {
		t.Errorf("Notes() 未排干：%q", again)
	}
}

// arguments.done 带完整值但身份帧全缺：完整值也要经合成块交付，不能随
// 流结束蒸发。
func TestStreamToolCallArgumentsDoneNoIdentity(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"x\":\"y\"}"}`,
		`{"type":"response.completed","response":{}}`)

	var args string
	for _, ev := range evs {
		if ev.Type == ir.EvToolInput {
			args += ev.Text
		}
	}
	if args != `{"x":"y"}` {
		t.Fatalf("done 携带的完整参数没交付：%q", args)
	}
}

// 对照组：added 正常到达的流不缓存、不合成、不报注记。
func TestStreamToolCallNormalFlowNoSynth(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.created","response":{"id":"r1","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call",`+
			`"call_id":"call_9","name":"f","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"k\":"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"k\":2}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call",`+
			`"call_id":"call_9","name":"f","arguments":"{\"k\":2}"}}`,
		`{"type":"response.completed","response":{}}`)

	var args string
	var tu *ir.ToolUse
	for _, ev := range evs {
		switch ev.Type {
		case ir.EvBlockStart:
			if ev.Block.Type == ir.BlockToolUse {
				tu = ev.Block.ToolUse
			}
		case ir.EvToolInput:
			args += ev.Text
		}
	}
	if tu == nil || tu.ID != "call_9" || tu.Name != "f" {
		t.Fatalf("正常流块身份不对：%+v", tu)
	}
	if args != `{"k":2}` {
		t.Fatalf("正常流参数拼错：%q", args)
	}
	if notes := dec.(interface{ Notes() []string }).Notes(); len(notes) != 0 {
		t.Errorf("正常流不该有注记：%q", notes)
	}
}
