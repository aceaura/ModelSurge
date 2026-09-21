package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 上游只发了半截正文、一个 finish_reason 都没给就断了：Finish 补的终止事件
// 必须是中断档。MapFinishReason("") 落到 default 会给出 end_turn，正是
// 「断流被伪装成正常完成」的来源，所以这里不能走映射。
func TestStreamDecodeAbortedStream(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	if _, err := dec.Feed("", `{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"content":"半截"}}]}`); err != nil {
		t.Fatal(err)
	}
	var delta *ir.Event
	evs := dec.Finish()
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

// 上游给过 finish_reason 就按它来，Finish 不得改写。
func TestStreamDecodeFinishKeepsUpstreamReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   ir.StopReason
	}{
		{"stop", ir.StopEndTurn},
		{"length", ir.StopMaxTokens},
		{"tool_calls", ir.StopToolUse},
		{"content_filter", ir.StopRefusal},
	} {
		dec := codec{}.NewStreamDecoder()
		if _, err := dec.Feed("", `{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"`+tc.reason+`"}]}`); err != nil {
			t.Fatal(err)
		}
		got := ir.StopReason("")
		for _, ev := range dec.Finish() {
			if ev.Type == ir.EvMessageDelta {
				got = ev.StopReason
			}
		}
		if got != tc.want {
			t.Errorf("finish_reason=%q -> %q, want %q", tc.reason, got, tc.want)
		}
	}
}

// 上游给了空串 finish_reason（有些代理会这么发）：这不算收到完成信号，
// 仍按中断处理。认成 stop 档就等于把代理的空字段读成「模型说完了」。
func TestStreamDecodeEmptyFinishReasonIsAborted(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	if _, err := dec.Feed("", `{"id":"c1","model":"gpt","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":""}]}`); err != nil {
		t.Fatal(err)
	}
	for _, ev := range dec.Finish() {
		if ev.Type == ir.EvMessageDelta && ev.StopReason != ir.StopAborted {
			t.Fatalf("空 finish_reason 被认成 %q", ev.StopReason)
		}
	}
}

// 流未开始时 Finish 不得造事件。
func TestStreamDecodeFinishWithoutStartIsEmpty(t *testing.T) {
	if evs := (codec{}).NewStreamDecoder().Finish(); len(evs) != 0 {
		t.Fatalf("未开始的流被补了事件：%+v", evs)
	}
}

// OpenAI 没有中断档，按输出不完整的最近档 length 表达。
func TestUnmapFinishReasonAborted(t *testing.T) {
	if got := UnmapFinishReason(ir.StopAborted); got != "length" {
		t.Fatalf("aborted -> %q, want length", got)
	}
	// 被借的那一档本身也要钉住：length 若塌成 stop，中断与截断会一起
	// 变成「正常说完」，而只断言 aborted -> length 的测试照样全绿。
	if got := UnmapFinishReason(ir.StopMaxTokens); got != "length" {
		t.Errorf("max_tokens -> %q, want length", got)
	}
	if got := UnmapFinishReason(ir.StopEndTurn); got != "stop" {
		t.Fatalf("end_turn 档被带歪：%q", got)
	}
}

// 编码器兜底：上游没走到 message_stop，收尾 chunk 不能写 stop。
func TestStreamEncodeFinishAborted(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "gpt"}); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, fr := range enc.Finish() {
		out = append(out, fr...)
	}
	s := string(out)
	if !strings.Contains(s, `"finish_reason":"length"`) {
		t.Errorf("兜底 chunk 没按中断档收尾：\n%s", s)
	}
	if strings.Contains(s, `"finish_reason":"stop"`) {
		t.Errorf("兜底 chunk 伪造了正常完成：\n%s", s)
	}
	// [DONE] 还是要发：客户端等不到它会一直挂着
	if !strings.Contains(s, "data: [DONE]") {
		t.Errorf("缺 [DONE]：\n%s", s)
	}
}

// 正常收尾后不得重复补帧。
func TestStreamEncodeFinishAfterNormalStop(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
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

// 非流式响应的中断档同样要落到 JSON。
func TestEncodeResponseAborted(t *testing.T) {
	body, err := codec{}.EncodeResponse(&ir.Response{
		ID: "c1", Model: "gpt", StopReason: ir.StopAborted,
		Content: []ir.Block{{Type: ir.BlockText, Text: "半截"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"finish_reason":"length"`) {
		t.Fatalf("非流式响应没带中断档：%s", body)
	}
}
