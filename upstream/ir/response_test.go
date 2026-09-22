package ir

import (
	"testing"
)

// 正常聚合：完整事件序列 -> Response。
func TestAggregator_HappyPath(t *testing.T) {
	a := NewAggregator()
	events := []Event{
		{Type: EvMessageStart, MessageID: "m1", Model: "test"},
		{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockThinking, Thinking: &Thinking{}}},
		{Type: EvThinkingDelta, Index: 0, Text: "想"},
		{Type: EvSigDelta, Index: 0, Text: "sig1"},
		{Type: EvBlockStop, Index: 0},
		{Type: EvBlockStart, Index: 1, Block: &Block{Type: BlockText}},
		{Type: EvTextDelta, Index: 1, Text: "Hello"},
		{Type: EvTextDelta, Index: 1, Text: " World"},
		{Type: EvBlockStop, Index: 1},
		{Type: EvMessageDelta, StopReason: StopEndTurn, Usage: &Usage{OutputTokens: 3}},
		{Type: EvMessageStop},
	}
	for _, ev := range events {
		a.Feed(ev)
	}
	resp, err := a.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "m1" || resp.Model != "test" {
		t.Errorf("meta = %q %q", resp.ID, resp.Model)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("blocks = %d", len(resp.Content))
	}
	if resp.Content[0].Thinking.Text != "想" || resp.Content[0].Thinking.Signature != "sig1" {
		t.Errorf("thinking = %+v", resp.Content[0].Thinking)
	}
	if resp.Content[1].Text != "Hello World" {
		t.Errorf("text = %q", resp.Content[1].Text)
	}
	if resp.StopReason != StopEndTurn || resp.Usage.OutputTokens != 3 {
		t.Errorf("stop/usage = %q %+v", resp.StopReason, resp.Usage)
	}
}

// 断流截断：未闭合的 tool_use 块由 Finish 冲刷，残缺 JSON 保留原文，
// 不能伪装成合法空对象交给下游执行。
func TestAggregator_TruncatedToolJSON(t *testing.T) {
	a := NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1"})
	a.Feed(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockToolUse, ToolUse: &ToolUse{ID: "c1", Name: "f"}}})
	a.Feed(Event{Type: EvToolInput, Index: 0, Text: `{"city": "Par`}) // 被截断
	// 没有 block_stop / message_stop —— 模拟断流
	resp, _ := a.Finish()
	if len(resp.Content) != 1 {
		t.Fatalf("blocks = %d", len(resp.Content))
	}
	tu := resp.Content[0].ToolUse
	if string(tu.Input) != `{"city": "Par` {
		t.Errorf("truncated input = %s, want original fragment", tu.Input)
	}
}

// EvError 终止聚合并透出错误。
func TestAggregator_Error(t *testing.T) {
	a := NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1"})
	if a.Feed(Event{Type: EvError, Err: NewHTTPError(429, "rate limited")}) {
		t.Fatal("Feed after error should return false")
	}
	_, err := a.Finish()
	if err == nil || err.StatusCode != 429 {
		t.Fatalf("err = %v", err)
	}
}
