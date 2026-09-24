package openairesponses

import (
	"strings"
	"testing"
)

// R106-A1 解码侧：responses 流式进度帧与未知事件型的损耗注记。
// response.queued / response.in_progress / *_call.* 进度事件（in_progress、
// searching、completed、partial_image、mcp_call_arguments 增量等）没有 IR
// 事件对应物，终态内容随 done 帧完整到达——忽略合法，但同族转发也丢帧，
// 必须计数报出；解码器不认识的事件型另立一账，不能与进度帧混报。

// 进度帧分类计数：queued/in_progress/_call.* 全部归入进度帧账，一条注记
// 报总数；不产生任何 IR 事件。
func TestProgressFramesCountedNotSilent(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.queued","response":{"id":"resp_1"}}`,
		`{"type":"response.in_progress","response":{"id":"resp_1"}}`,
		`{"type":"response.web_search_call.in_progress","output_index":0,"item_id":"ws_1"}`,
		`{"type":"response.mcp_call_arguments.delta","output_index":1,"item_id":"mcp_1","delta":"{}"}`,
	)
	if len(evs) != 0 {
		t.Errorf("进度帧不该产出 IR 事件：%+v", evs)
	}
	n := dec.(interface{ Notes() []string })
	notes := n.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "4 hosted-call progress frame(s)") {
		t.Fatalf("进度帧计数注记不对：%q", notes)
	}
	if again := n.Notes(); len(again) != 0 {
		t.Errorf("Notes() 未排干：%q", again)
	}
}

// 未知事件型与进度帧分账：本仓连语义都不知道的帧单独计数，措辞不能与
// 「有对应物但装不下的过程信号」混同。
func TestUnknownEventTypeCountedSeparately(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs := feedAll(t, dec,
		`{"type":"response.web_search_call.searching","output_index":0,"item_id":"ws_1"}`,
		`{"type":"response.frobnicate.started","response":{"id":"resp_1"}}`,
		`{"type":"response.output_text.frobbed","output_index":0}`,
	)
	if len(evs) != 0 {
		t.Errorf("未知/进度帧都不该产出 IR 事件：%+v", evs)
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	if len(notes) != 2 {
		t.Fatalf("进度帧与未知型应各报一条：%q", notes)
	}
	if !strings.Contains(notes[0], "1 hosted-call progress frame(s)") {
		t.Errorf("进度帧注记不对：%q", notes[0])
	}
	if !strings.Contains(notes[1], "2 stream event(s)") {
		t.Errorf("未知事件型注记不对：%q", notes[1])
	}
}

// 正常流零注记：已知事件型走完一轮不该留下任何计数。
func TestKnownEventsLeaveNoNotes(t *testing.T) {
	dec := New().NewStreamDecoder()
	feedAll(t, dec,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5","created_at":1}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
	)
	if notes := dec.(interface{ Notes() []string }).Notes(); len(notes) != 0 {
		t.Errorf("已知事件型全程不该有注记：%q", notes)
	}
}
