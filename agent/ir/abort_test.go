package ir

import "testing"

func TestStopReasonIncomplete(t *testing.T) {
	for _, tc := range []struct {
		stop StopReason
		want bool
	}{
		{StopEndTurn, false},
		{StopToolUse, false},
		{StopStopSequence, false},
		{StopRefusal, false},
		{StopMaxTokens, true},
		{StopPauseTurn, true},
		{StopAborted, true},
		{StopReason(""), false},
	} {
		if got := tc.stop.Incomplete(); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.stop, got, tc.want)
		}
	}
}

// StopAborted 不能与任何原生档撞值：撞上会让协议映射把中断认成上游真给的档。
func TestStopAbortedDistinct(t *testing.T) {
	for _, s := range []StopReason{StopEndTurn, StopMaxTokens, StopToolUse, StopRefusal, StopStopSequence, StopPauseTurn} {
		if s == StopAborted {
			t.Fatalf("StopAborted collides with %q", s)
		}
	}
}

// 聚合路径（上游流式、客户端非流式）同样要把中断档带到 Response 上。
func TestAggregatorCarriesAbortedStop(t *testing.T) {
	a := NewAggregator()
	for _, ev := range []Event{
		{Type: EvMessageStart, MessageID: "m1"},
		{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}},
		{Type: EvTextDelta, Index: 0, Text: "半截"},
		{Type: EvMessageDelta, StopReason: StopAborted},
	} {
		a.Feed(ev)
	}
	resp, err := a.Finish()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StopReason != StopAborted {
		t.Fatalf("stop reason: got %q want %q", resp.StopReason, StopAborted)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "半截" {
		t.Fatalf("content lost: %+v", resp.Content)
	}
}
