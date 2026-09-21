package relay

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 上游整份 JSON -> 客户端 SSE 这条路径上，引用必须改走 EvCitation 且排在正文之后：
// 留在 block_start 上会让编码器在正文还没发出时就写出偏移量，反推全部失败。
func TestEventsFromResponseEmitsCitationAfterText(t *testing.T) {
	resp := &ir.Response{ID: "msg_1", Model: "m", Content: []ir.Block{{
		Type: ir.BlockText, Text: "北京今天晴，明天有雨。",
		Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨"}},
	}}}
	evs := EventsFromResponse(resp)
	textAt, citeAt, startAt := -1, -1, -1
	for i, ev := range evs {
		switch ev.Type {
		case ir.EvBlockStart:
			startAt = i
			if len(ev.Block.Citations) != 0 {
				t.Errorf("block_start 上还挂着引用：%+v", ev.Block.Citations)
			}
		case ir.EvTextDelta:
			textAt = i
		case ir.EvCitation:
			citeAt = i
			if got := ev.Citations; len(got) != 1 || got[0].URL != "https://w" {
				t.Errorf("引用内容不对：%+v", got)
			}
			if ev.Index != 0 {
				t.Errorf("块序号 = %d, want 0", ev.Index)
			}
		}
	}
	if citeAt == -1 {
		t.Fatal("引用在这条路径上整块丢失")
	}
	if !(startAt < textAt && textAt < citeAt) {
		t.Fatalf("顺序不对：block_start=%d text=%d citation=%d", startAt, textAt, citeAt)
	}
}

// 无引用时不得凭空插入 EvCitation：空事件会让编码器发出无意义的帧。
func TestEventsFromResponseNoCitationEvent(t *testing.T) {
	evs := EventsFromResponse(&ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}})
	for _, ev := range evs {
		if ev.Type == ir.EvCitation {
			t.Fatalf("无引用却发了 EvCitation：%+v", ev)
		}
	}
}

// 拒绝块同样用 Text 承载，其上的引用也要照样投影出来。
func TestEventsFromResponseCitationOnRefusal(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{
		Type: ir.BlockRefusal, Text: "policy says no",
		Citations: []ir.Citation{{URL: "https://p", CitedText: "policy"}},
	}}}
	var found bool
	for _, ev := range EventsFromResponse(resp) {
		if ev.Type == ir.EvCitation {
			found = true
		}
	}
	if !found {
		t.Fatal("拒绝块上的引用被丢掉了")
	}
}
