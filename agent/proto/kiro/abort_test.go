package kiro

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 有正文但没有完成信号：上游把话说到一半被截断，已有的 max_tokens 档。
func TestDecoder_ContentWithoutFinishIsTruncated(t *testing.T) {
	evs := summary(driveDecoder(t, `{"content":"半截"}`))
	wantEvents(t, evs,
		"start", "block_start:text", "text:半截", "block_stop",
		"delta:max_tokens", "stop",
	)
}

// 有完成信号（contextUsagePercentage）就是正常收尾，不得报中断。
func TestDecoder_FinishSignalIsEndTurn(t *testing.T) {
	evs := summary(driveDecoder(t, `{"content":"完整"}`, `{"contextUsagePercentage":12.5}`))
	wantEvents(t, evs,
		"start", "block_start:text", "text:完整", "block_stop",
		"delta:end_turn", "stop",
	)
}

// 有工具调用时按 tool_use 收尾：工具调用本身就是「这一轮交给客户端」的
// 完成信号，不能因为没有 usage 就报成中断。
func TestDecoder_ToolCallStopWinsOverAbort(t *testing.T) {
	evs := driveDecoder(t, `{"name":"get_time","toolUseId":"call_1","input":"{}","stop":true}`)
	got := ir.StopReason("")
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopToolUse {
		t.Fatalf("停止原因 = %q, want %q", got, ir.StopToolUse)
	}
}

// usage 事件本身就是完成信号：上游把账结了就算说完了，即使一个字都没回。
// 报中断档会让客户端把一次已计费的空回答重试掉。
func TestDecoder_UsageOnlyIsNotAborted(t *testing.T) {
	evs := driveDecoder(t, `{"usage":{"cacheReadInputTokens":7}}`)
	got := ir.StopReason("")
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopEndTurn {
		t.Fatalf("停止原因 = %q, want %q", got, ir.StopEndTurn)
	}
}

// 中断档下 ContentTruncated 必须为假：它的语义是「有正文被截断」，
// 空流没有正文可关联，误报会让 relay 把不存在的截断内容记进账。
func TestDecoder_AbortedIsNotContentTruncated(t *testing.T) {
	dec := &streamDecoder{}
	dec.Finish()
	if dec.ContentTruncated() {
		t.Fatal("空流被当成了内容截断")
	}
	if dec.TruncatedContent() != "" {
		t.Fatalf("空流报出了截断正文：%q", dec.TruncatedContent())
	}
}
