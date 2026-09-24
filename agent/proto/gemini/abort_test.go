package gemini

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// Gemini 既没有续跑也没有中断语义，按输出不完整的最近档 MAX_TOKENS 表达。
func TestUnmapFinishReasonAborted(t *testing.T) {
	if got := UnmapFinishReason(ir.StopAborted); got != "MAX_TOKENS" {
		t.Fatalf("aborted -> %q, want MAX_TOKENS", got)
	}
	// 中断档借的是 MAX_TOKENS 这一档，被借的那一档本身也得钉住：
	// 它若塌成 STOP，中断与截断会一起变成「正常说完」而测试全绿。
	if got := UnmapFinishReason(ir.StopMaxTokens); got != "MAX_TOKENS" {
		t.Errorf("max_tokens -> %q, want MAX_TOKENS", got)
	}
	if got := UnmapFinishReason(ir.StopEndTurn); got != "STOP" {
		t.Fatalf("end_turn 档被带歪：%q", got)
	}
}

// 编码器兜底：上游没给终止 chunk 就断了，收尾 chunk 不能写 STOP。
func TestStreamEncodeFinishAborted(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini-3-pro"},
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
	if !strings.Contains(s, `"finishReason":"MAX_TOKENS"`) {
		t.Errorf("兜底 chunk 没按中断档收尾：\n%s", s)
	}
	if strings.Contains(s, `"finishReason":"STOP"`) {
		t.Errorf("兜底 chunk 伪造了正常完成：\n%s", s)
	}
}

// 正常收尾后不得重复补终止 chunk。
func TestStreamEncodeFinishAfterNormalStop(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini-3-pro"},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
	} {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	for _, fr := range enc.Finish() {
		if strings.Contains(string(fr), `"finishReason"`) {
			t.Fatalf("正常收尾后又补了终止 chunk：%s", fr)
		}
	}
}

// 兜底时未闭合的 functionCall 仍要冲刷出去，且之后才是终止 chunk：
// 顺序颠倒会让客户端在看到工具调用前就认为这一轮结束了。
func TestStreamEncodeFinishFlushesToolBeforeTerminator(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini-3-pro"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "call_1", Name: "get_time"}}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"tz":"UTC"}`},
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
	call := strings.Index(s, `"functionCall"`)
	fin := strings.Index(s, `"finishReason":"MAX_TOKENS"`)
	if call < 0 || fin < 0 {
		t.Fatalf("兜底缺 functionCall 或终止 chunk：\n%s", s)
	}
	if call > fin {
		t.Fatalf("终止 chunk 排在了工具调用之前：\n%s", s)
	}
}

// 非流式响应的中断档同样要落到 JSON。
func TestEncodeResponseAborted(t *testing.T) {
	body, err := codec{}.EncodeResponse(&ir.Response{
		ID: "r1", Model: "gemini-3-pro", StopReason: ir.StopAborted,
		Content: []ir.Block{{Type: ir.BlockText, Text: "半截"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"finishReason":"MAX_TOKENS"`) {
		t.Fatalf("非流式响应没带中断档：%s", body)
	}
}

// R104 终止守卫：错误帧已是终止帧（EvError 置 finished），之后再发
// message_delta 的 finishChunk 就是把失败伪装成正常结束。
func TestStreamEncodeSuppressesFinishChunkAfterError(t *testing.T) {
	enc := New().NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeOverloaded, Message: "x"}}); err != nil {
		t.Fatal(err)
	}
	if frames, _ := enc.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn}); len(frames) != 0 {
		t.Errorf("错误之后又发出 finish chunk：%q", frames)
	}
}
