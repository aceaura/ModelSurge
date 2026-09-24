package ir

import (
	"strings"
	"testing"
)

// 托管调用的查询串走 EvToolInput 增量通道（responses 合成的 server_tool_use
// 与 anthropic 流式 server_tool_use 都是这个形态：开块不带 input）。聚合器
// 必须在关块时把累积的查询串填回 ServerToolUse.Input——漏登记会让聚合路径
// （上游流式、客户端非流式）里查询串整段蒸发。
func TestAggregatorServerToolInputAccumulates(t *testing.T) {
	a := NewAggregator()
	for _, ev := range []Event{
		{Type: EvMessageStart, MessageID: "m1", Model: "m"},
		{Type: EvBlockStart, Index: 0, Block: &Block{
			Type:          BlockServerToolUse,
			ServerToolUse: &ServerToolUse{ID: "srvtoolu_1", Name: "web_search"},
		}},
		{Type: EvToolInput, Index: 0, Text: `{"query":"weather in `},
		{Type: EvToolInput, Index: 0, Text: `Paris"}`},
		{Type: EvBlockStop, Index: 0},
		{Type: EvMessageDelta, StopReason: StopEndTurn},
		{Type: EvMessageStop},
	} {
		a.Feed(ev)
	}
	resp, err := a.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ServerToolUse == nil {
		t.Fatalf("托管调用块丢失：%+v", resp.Content)
	}
	got := string(resp.Content[0].ServerToolUse.Input)
	if got != `{"query":"weather in Paris"}` {
		t.Errorf("查询串没有聚回 input：%q", got)
	}
	if notes := a.Notes(); len(notes) != 0 {
		// 服务端产出的查询串不做参数规整，合法 JSON 也不许进规整注记。
		t.Errorf("托管调用不该触发规整注记：%q", notes)
	}
}

// 开块已带完整 input（非流式回放）时不登记累积器：后来的 EvToolInput 不得
// 覆盖或追加，否则同一段查询会被拼两遍。
func TestAggregatorServerToolSeededInputNotDoubled(t *testing.T) {
	a := NewAggregator()
	for _, ev := range []Event{
		{Type: EvMessageStart, MessageID: "m1", Model: "m"},
		{Type: EvBlockStart, Index: 0, Block: &Block{
			Type: BlockServerToolUse,
			ServerToolUse: &ServerToolUse{
				ID: "srvtoolu_1", Name: "web_search", Input: []byte(`{"query":"q"}`),
			},
		}},
		{Type: EvToolInput, Index: 0, Text: `{"query":"q"}`},
		{Type: EvBlockStop, Index: 0},
		{Type: EvMessageDelta, StopReason: StopEndTurn},
		{Type: EvMessageStop},
	} {
		a.Feed(ev)
	}
	resp, err := a.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got := string(resp.Content[0].ServerToolUse.Input)
	if !strings.Contains(got, `"query":"q"`) || strings.Count(got, "query") != 1 {
		t.Errorf("开块自带 input 被增量污染：%q", got)
	}
}
