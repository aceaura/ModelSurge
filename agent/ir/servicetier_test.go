package ir

import "testing"

// R64：聚合器收档位回显——EvMessageStart 首帧携带与 EvMessageDelta 晚到两档。
func TestAggregatorServiceTier(t *testing.T) {
	a := NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "g", ServiceTier: "standard"})
	a.Feed(Event{Type: EvMessageStop})
	resp, _ := a.Finish()
	if resp.ServiceTier != "standard" {
		t.Errorf("首帧回显 = %q", resp.ServiceTier)
	}

	a = NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "g"})
	a.Feed(Event{Type: EvMessageDelta, StopReason: StopEndTurn, ServiceTier: "flex"})
	a.Feed(Event{Type: EvMessageStop})
	resp, _ = a.Finish()
	if resp.ServiceTier != "flex" {
		t.Errorf("晚到回显 = %q", resp.ServiceTier)
	}

	// 全缺省：不多一档。
	a = NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1", Model: "g"})
	a.Feed(Event{Type: EvMessageStop})
	resp, _ = a.Finish()
	if resp.ServiceTier != "" {
		t.Errorf("缺省回显 = %q", resp.ServiceTier)
	}
}
