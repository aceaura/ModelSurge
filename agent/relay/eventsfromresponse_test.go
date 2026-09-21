package relay

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// EventsFromResponse 是「上游给整份 JSON、客户端要 SSE」那条兜底路径上
// 唯一的投影点，此前零测试覆盖。它漏一个字段就等于该维度在这条路径上不存在，
// 而另一条（上游本就流式）路径照样全绿，故障只在特定目标组合下出现。

func TestEventsFromResponseCarriesStopDimensions(t *testing.T) {
	resp := &ir.Response{
		ID: "msg_1", Model: "m",
		Content:      []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		StopReason:   ir.StopStopSequence,
		StopSequence: "END",
		Usage:        ir.Usage{InputTokens: 3, OutputTokens: 2},
	}
	var delta *ir.Event
	for _, ev := range EventsFromResponse(resp) {
		if ev.Type == ir.EvMessageDelta {
			e := ev
			delta = &e
		}
	}
	if delta == nil {
		t.Fatal("没合成 message_delta")
	}
	if delta.StopReason != ir.StopStopSequence {
		t.Errorf("StopReason = %q, want stop_sequence", delta.StopReason)
	}
	if delta.StopSequence != "END" {
		t.Errorf("StopSequence = %q, 命中的序列在这条路径上丢了", delta.StopSequence)
	}
	if delta.Usage == nil || delta.Usage.OutputTokens != 2 {
		t.Errorf("Usage = %+v", delta.Usage)
	}
}

// 事件序列必须是下游编码器可收尾的：起头 message_start、收尾
// message_delta + message_stop。
func TestEventsFromResponseIsWellFormed(t *testing.T) {
	evs := EventsFromResponse(&ir.Response{
		ID: "msg_1", Model: "m",
		Content:    []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		StopReason: ir.StopEndTurn,
	})
	if len(evs) < 3 {
		t.Fatalf("事件数 = %d，太少：%+v", len(evs), evs)
	}
	if evs[0].Type != ir.EvMessageStart {
		t.Errorf("首个事件 = %q, want message_start", evs[0].Type)
	}
	if got := evs[len(evs)-2].Type; got != ir.EvMessageDelta {
		t.Errorf("倒数第二个 = %q, want message_delta", got)
	}
	if got := evs[len(evs)-1].Type; got != ir.EvMessageStop {
		t.Errorf("末个事件 = %q, want message_stop", got)
	}
}

// 未命中停止串时不能凭空造一条：下游 anthropic 编码器会把它原样写进
// message_delta，客户端会以为命中了一条空串。
func TestEventsFromResponseOmitsUnmatchedStopSequence(t *testing.T) {
	for _, ev := range EventsFromResponse(&ir.Response{StopReason: ir.StopEndTurn}) {
		if ev.Type == ir.EvMessageDelta && ev.StopSequence != "" {
			t.Errorf("凭空造了 StopSequence = %q", ev.StopSequence)
		}
	}
}
