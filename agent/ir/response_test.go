package ir

import (
	"encoding/json"
	"strings"
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

// 断流截断：未闭合的 tool_use 块由 Finish 冲刷。残缺 JSON 不能清空成 {}——
// 空参数会让工具不带参数执行；原文挪进 RawArgsKey 键位保真。
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
	var m map[string]any
	if err := json.Unmarshal(tu.Input, &m); err != nil {
		t.Fatalf("规整结果不是合法 JSON 对象: %s", tu.Input)
	}
	raw, ok := m[RawArgsKey]
	if !ok {
		t.Fatalf("截断原文没挪进 %s: %s", RawArgsKey, tu.Input)
	}
	if s, _ := raw.(string); s != `{"city": "Par` {
		t.Errorf("原文 = %v, 截断片段丢了", raw)
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

// 聚合期挪键记账：截断的工具参数在 BlockStop（或 Finish 冲刷残余块）时被
// 挪进 RawArgsKey，这条损耗只发生在聚合器，codec 看到的是规整后的合法对象——
// 注记必须由 Aggregator.Notes 带出，否则非流式客户端路径上它彻底不可见。
func TestAggregatorNotesOnMalformedToolArgs(t *testing.T) {
	feed := func(a *Aggregator) {
		a.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "m"})
		a.Feed(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockToolUse, ToolUse: &ToolUse{ID: "c1", Name: "f"}}})
		a.Feed(Event{Type: EvToolInput, Index: 0, Text: `{"a": 1`})
	}
	// 正常 BlockStop 收尾。
	a := NewAggregator()
	feed(a)
	a.Feed(Event{Type: EvBlockStop, Index: 0})
	a.Feed(Event{Type: EvMessageDelta, StopReason: StopToolUse})
	a.Feed(Event{Type: EvMessageStop})
	if _, err := a.Finish(); err != nil {
		t.Fatalf("Finish err=%v", err)
	}
	notes := a.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "rewrapped 1 malformed tool call argument(s)") {
		t.Fatalf("BlockStop 路径挪键未报：%v", notes)
	}
	if again := a.Notes(); len(again) != 0 {
		t.Errorf("Notes 未排干：%v", again)
	}
	// 断流由 Finish 冲刷残余块，同样要记账。
	b := NewAggregator()
	feed(b)
	if _, err := b.Finish(); err != nil {
		t.Fatalf("Finish err=%v", err)
	}
	if notes := b.Notes(); len(notes) != 1 {
		t.Errorf("Finish 冲刷路径挪键未报：%v", notes)
	}
}

// 合法参数不记账：Notes 默认为空，安静是常态。
func TestAggregatorNotesSilentOnValidArgs(t *testing.T) {
	a := NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "m"})
	a.Feed(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockToolUse, ToolUse: &ToolUse{ID: "c1", Name: "f"}}})
	a.Feed(Event{Type: EvToolInput, Index: 0, Text: `{"a": 1}`})
	a.Feed(Event{Type: EvBlockStop, Index: 0})
	a.Feed(Event{Type: EvMessageDelta, StopReason: StopToolUse})
	a.Feed(Event{Type: EvMessageStop})
	a.Finish()
	if notes := a.Notes(); len(notes) != 0 {
		t.Errorf("合法参数误报：%v", notes)
	}
}

func TestAggregatorCustomToolPreservesFreeFormInput(t *testing.T) {
	for _, closeBlock := range []bool{true, false} {
		a := NewAggregator()
		a.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "m"})
		a.Feed(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockToolUse, ToolUse: &ToolUse{ID: "c1", Name: "shell", Kind: ToolCustom}}})
		a.Feed(Event{Type: EvToolInput, Index: 0, Text: "echo "})
		a.Feed(Event{Type: EvToolInput, Index: 0, Text: "hi"})
		if closeBlock {
			a.Feed(Event{Type: EvBlockStop, Index: 0})
		}
		resp, err := a.Finish()
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Content) != 1 || resp.Content[0].ToolUse == nil {
			t.Fatalf("close=%v content=%+v", closeBlock, resp.Content)
		}
		call := resp.Content[0].ToolUse
		if call.Kind != ToolCustom || call.InputText != "echo hi" || string(call.Input) != `{"input":"echo hi"}` {
			t.Fatalf("close=%v call=%+v input=%s", closeBlock, call, call.Input)
		}
		if notes := a.Notes(); len(notes) != 0 {
			t.Fatalf("close=%v custom input misreported as malformed: %v", closeBlock, notes)
		}
	}
}
