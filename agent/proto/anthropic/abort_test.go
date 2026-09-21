package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 上游只发了 message_start + 半截正文就断了：Finish 补的终止事件必须是中断档。
// 报 end_turn 会让客户端把半截输出当成最终答案提交，而正确动作是重试。
func TestStreamDecodeAbortedStream(t *testing.T) {
	dec := New().NewStreamDecoder()
	for _, ev := range []struct{ event, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude"}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半截"}}`},
	} {
		if _, err := dec.Feed(ev.event, ev.data); err != nil {
			t.Fatal(err)
		}
	}
	evs := dec.Finish()
	var delta *ir.Event
	for i := range evs {
		if evs[i].Type == ir.EvMessageDelta {
			delta = &evs[i]
		}
	}
	if delta == nil {
		t.Fatalf("Finish 没补终止事件：%+v", evs)
	}
	if delta.StopReason != ir.StopAborted {
		t.Fatalf("停止原因 = %q, want %q", delta.StopReason, ir.StopAborted)
	}
}

// 上游给过 message_delta 就是正常收尾，Finish 不得再改写停止原因。
func TestStreamDecodeNormalStopNotOverwritten(t *testing.T) {
	dec := New().NewStreamDecoder()
	if _, err := dec.Feed("message_start", `{"type":"message_start","message":{"id":"m","model":"c"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[0].StopReason != ir.StopEndTurn {
		t.Fatalf("正常档被改写：%+v", evs)
	}
	for _, ev := range dec.Finish() {
		if ev.Type == ir.EvMessageDelta {
			t.Fatalf("Finish 重复补了终止事件：%+v", ev)
		}
	}
}

// 流未开始（一个事件都没解出来）时 Finish 不得凭空造终止事件：
// 此时上游连 message_start 都没给，下游还没有任何消息可收尾。
func TestStreamDecodeFinishWithoutStartIsEmpty(t *testing.T) {
	if evs := New().NewStreamDecoder().Finish(); len(evs) != 0 {
		t.Fatalf("未开始的流被补了事件：%+v", evs)
	}
}

// Anthropic 没有「中断」档，按输出不完整的最近档 max_tokens 表达。
func TestUnmapStopReasonAborted(t *testing.T) {
	if got := UnmapStopReason(ir.StopAborted); got != "max_tokens" {
		t.Fatalf("aborted -> %q, want max_tokens", got)
	}
	if got := UnmapStopReason(ir.StopEndTurn); got != "end_turn" {
		t.Fatalf("end_turn 档被带歪：%q", got)
	}
}

// Anthropic 线上没有 aborted 这个值，解码时不能认它——否则同协议往返会把
// 客户端伪造的 stop_reason 当成本仓的内部中断档。
func TestMapStopReasonRejectsAborted(t *testing.T) {
	if got := MapStopReason("aborted"); got != ir.StopEndTurn {
		t.Fatalf("上游 aborted 被认成 %q", got)
	}
}

// 编码器兜底：上游没给终止事件就断了，收尾帧不能写 end_turn。
func TestStreamEncodeFinishAborted(t *testing.T) {
	enc := New().NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "半截"}); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, fr := range enc.Finish() {
		out = append(out, fr...)
	}
	s := string(out)
	if !strings.Contains(s, `"stop_reason":"max_tokens"`) {
		t.Errorf("兜底帧没按中断档收尾：\n%s", s)
	}
	if strings.Contains(s, `"stop_reason":"end_turn"`) {
		t.Errorf("兜底帧伪造了正常完成：\n%s", s)
	}
	// 未闭合的块仍要关，否则客户端解析器一直等 content_block_stop
	if !strings.Contains(s, `"type":"content_block_stop"`) {
		t.Errorf("未闭块没关：\n%s", s)
	}
	if !strings.Contains(s, `"type":"message_stop"`) {
		t.Errorf("缺 message_stop：\n%s", s)
	}
}

// 上游正常收尾时编码器不得再补一份终止帧。
func TestStreamEncodeFinishAfterNormalStop(t *testing.T) {
	enc := New().NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	} {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	if fr := enc.Finish(); len(fr) != 0 {
		t.Fatalf("正常收尾后又补了帧：%s", fr)
	}
}

// 非流式响应的中断档同样要按 max_tokens 落到 JSON 里。
func TestEncodeResponseAborted(t *testing.T) {
	body, err := codec{}.EncodeResponse(&ir.Response{
		ID: "msg_1", Model: "claude", StopReason: ir.StopAborted,
		Content: []ir.Block{{Type: ir.BlockText, Text: "半截"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"stop_reason":"max_tokens"`) {
		t.Fatalf("非流式响应没带中断档：%s", body)
	}
}
