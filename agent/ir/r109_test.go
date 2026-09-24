package ir

import (
	"testing"
)

// R109-A1 聚合器收下上游创建时间：EvMessageStart 携带的 Created 落进
// Response.Created，非流式化后与时间戳回显同一口径。
func TestR109AggregatorCreated(t *testing.T) {
	a := NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "m", Created: 1750000000})
	a.Feed(Event{Type: EvMessageDelta, StopReason: StopEndTurn})
	a.Feed(Event{Type: EvMessageStop})
	resp, err := a.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Created != 1750000000 {
		t.Fatalf("Created = %d", resp.Created)
	}

	// 上游没给时保持零值（编码器回退本地钟，不伪造）。
	a2 := NewAggregator()
	a2.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "m"})
	a2.Feed(Event{Type: EvMessageDelta, StopReason: StopEndTurn})
	a2.Feed(Event{Type: EvMessageStop})
	resp2, err := a2.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if resp2.Created != 0 {
		t.Fatalf("零值被伪造：%d", resp2.Created)
	}
}

// R109-A6 聚合器收 message_delta 顶层的 context_management 回执：
// 流式聚合后与非流式 DecodeResponse 落同一个 IR 槽位。
func TestR109AggregatorContextMgmt(t *testing.T) {
	a := NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "m"})
	a.Feed(Event{Type: EvMessageDelta, StopReason: StopEndTurn,
		ContextMgmt: []byte(`{"applied_edits":[{"type":"clear_tool_uses_20250919"}]}`)})
	a.Feed(Event{Type: EvMessageStop})
	resp, err := a.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.AnthropicContextMgmt) != `{"applied_edits":[{"type":"clear_tool_uses_20250919"}]}` {
		t.Fatalf("AnthropicContextMgmt = %s", resp.AnthropicContextMgmt)
	}

	// 没带时保持空（编码侧不造键）。
	a2 := NewAggregator()
	a2.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "m"})
	a2.Feed(Event{Type: EvMessageDelta, StopReason: StopEndTurn})
	a2.Feed(Event{Type: EvMessageStop})
	resp2, err := a2.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp2.AnthropicContextMgmt) != 0 {
		t.Fatalf("缺席被伪造：%s", resp2.AnthropicContextMgmt)
	}
}
