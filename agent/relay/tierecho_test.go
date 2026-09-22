package relay

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R64：非流式上游 -> 流式客户端的合成事件流要带上档位回显，
// 否则这条兜底路径把它整条吃掉。
func TestEventsFromResponseCarriesServiceTier(t *testing.T) {
	evs := EventsFromResponse(&ir.Response{ID: "m1", Model: "g", ServiceTier: "batch"})
	if len(evs) == 0 || evs[0].Type != ir.EvMessageStart || evs[0].ServiceTier != "batch" {
		t.Fatalf("首事件未带回显：%+v", evs)
	}
}
