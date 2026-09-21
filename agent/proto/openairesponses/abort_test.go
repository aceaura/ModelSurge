package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 上游只发了 created + 半截正文就断了：Finish 补的终止事件必须是中断档。
func TestStreamDecodeAbortedStream(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	for _, data := range []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"半截"}`,
	} {
		if _, err := dec.Feed("", data); err != nil {
			t.Fatal(err)
		}
	}
	got := ir.StopReason("")
	for _, ev := range dec.Finish() {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopAborted {
		t.Fatalf("停止原因 = %q, want %q", got, ir.StopAborted)
	}
}

// 收到 response.completed 就是正常收尾，Finish 不得再改写也不得重复补。
func TestStreamDecodeFinishAfterCompleted(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	if _, err := dec.Feed("", `{"type":"response.created","response":{"id":"resp_1","model":"gpt"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("", `{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)
	if err != nil {
		t.Fatal(err)
	}
	got := ir.StopReason("")
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopEndTurn {
		t.Fatalf("正常完成被改写成 %q", got)
	}
	if rest := dec.Finish(); len(rest) != 0 {
		t.Fatalf("已完成的流又被补了事件：%+v", rest)
	}
}

// 上游明说截断（response.incomplete）时保留它自己的原因，不塌成中断档。
func TestStreamDecodeIncompleteKeepsReason(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	if _, err := dec.Feed("", `{"type":"response.created","response":{"id":"resp_1","model":"gpt"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("", `{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	got := ir.StopReason("")
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopRefusal {
		t.Fatalf("上游 content_filter -> %q, want %q", got, ir.StopRefusal)
	}
}

// 流未开始时 Finish 不得造事件。
func TestStreamDecodeFinishWithoutStartIsEmpty(t *testing.T) {
	if evs := (codec{}).NewStreamDecoder().Finish(); len(evs) != 0 {
		t.Fatalf("未开始的流被补了事件：%+v", evs)
	}
}

// Responses 有 incomplete 状态但没有「中断」原因，按 max_output_tokens 表达。
func TestUnmapIncompleteReasonAborted(t *testing.T) {
	if got := unmapIncompleteReason(ir.StopAborted); got != "max_output_tokens" {
		t.Fatalf("aborted -> %q, want max_output_tokens", got)
	}
	if got := unmapIncompleteReason(ir.StopEndTurn); got != "" {
		t.Fatalf("end_turn 被当成了截断：%q", got)
	}
}

// 编码器兜底：没收到 EvMessageDelta 就走 Finish，收尾帧必须是
// response.incomplete。发 response.completed 会被下游解码器（与官方 SDK）
// 按事件名读成正常完成——这是断流伪装的最后一道出口。
func TestStreamEncodeFinishAborted(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "半截"},
	} {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	var out []byte
	for _, fr := range enc.Finish() {
		out = append(out, fr...)
	}
	s := string(out)
	for _, want := range []string{
		`"type":"response.incomplete"`, `"status":"incomplete"`, `"reason":"max_output_tokens"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("兜底帧缺 %s：\n%s", want, s)
		}
	}
	if strings.Contains(s, `"type":"response.completed"`) {
		t.Errorf("兜底帧伪造了正常完成：\n%s", s)
	}
	// 已下发的正文仍要在终止帧的 output 里，否则 SDK 读 final response 拿到空
	if !strings.Contains(s, "半截") {
		t.Errorf("兜底帧丢了已下发正文：\n%s", s)
	}
}

// 正常收尾后不得重复补终止帧。
func TestStreamEncodeFinishAfterNormalStop(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt"},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	} {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	for _, fr := range enc.Finish() {
		if strings.Contains(string(fr), "response.completed") || strings.Contains(string(fr), "response.incomplete") {
			t.Fatalf("正常收尾后又补了终止帧：%s", fr)
		}
	}
}

// 非流式响应的中断档要落成 incomplete 状态。
func TestEncodeResponseAborted(t *testing.T) {
	body, err := codec{}.EncodeResponse(&ir.Response{
		ID: "resp_1", Model: "gpt", StopReason: ir.StopAborted,
		Content: []ir.Block{{Type: ir.BlockText, Text: "半截"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, `"status":"incomplete"`) || !strings.Contains(s, `"reason":"max_output_tokens"`) {
		t.Fatalf("非流式响应没带中断档：%s", s)
	}
}
