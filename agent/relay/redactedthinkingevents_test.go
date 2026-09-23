package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
)

// R90：「上游非流式、客户端流式」这条重放路径要把涂抹块整块带过去。它没有增量
// 形态，密文只能随 EvBlockStart 全量下发（与 server_tool_use /
// web_search_tool_result 同一处置）；给它补 delta 事件反而是无中生有。

func TestEventsFromResponseCarriesRedactedBlock(t *testing.T) {
	resp := &ir.Response{ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockRedactedThinking, RedactedData: r90Cipher},
			{Type: ir.BlockText, Text: "visible answer"},
		}}
	events := EventsFromResponse(resp)
	var got *ir.Block
	starts, stops := 0, 0
	for _, ev := range events {
		switch ev.Type {
		case ir.EvBlockStart:
			starts++
			if ev.Index == 0 {
				got = ev.Block
			}
		case ir.EvBlockStop:
			stops++
		case ir.EvTextDelta, ir.EvThinkingDelta, ir.EvSigDelta:
			if ev.Index == 0 {
				t.Errorf("涂抹块不该有增量事件：%s %q", ev.Type, ev.Text)
			}
		}
	}
	if starts != 2 || stops != 2 {
		t.Fatalf("块框架不配平：start=%d stop=%d", starts, stops)
	}
	if got == nil || got.Type != ir.BlockRedactedThinking || got.RedactedData != r90Cipher {
		t.Fatalf("块开始事件丢密文：%+v", got)
	}
	// 重放不得改动调用方持有的聚合响应（usage 估算与日志摘要还在读它）。
	if len(resp.Content) != 2 || resp.Content[0].RedactedData != r90Cipher {
		t.Errorf("原响应被就地改坏：%+v", resp.Content)
	}
}

// 端到端：重放出的事件流交给 anthropic 编码器，客户端要能在 content_block_start
// 里收到密文——否则这一轮之后客户端无从回传，Anthropic 的续话校验拒下一轮。
func TestEventsFromResponseRedactedReachesAnthropicStream(t *testing.T) {
	resp := &ir.Response{ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockRedactedThinking, RedactedData: r90Cipher},
			{Type: ir.BlockText, Text: "visible answer"},
		}}
	enc := proto.MustInbound("anthropic").NewStreamEncoder()
	var sb strings.Builder
	for _, ev := range EventsFromResponse(resp) {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s idx=%d): %v", ev.Type, ev.Index, err)
		}
		for _, fr := range frames {
			sb.Write(fr)
		}
	}
	for _, fr := range enc.Finish() {
		sb.Write(fr)
	}
	out := sb.String()
	if !strings.Contains(out, `"content_block":{"type":"redacted_thinking"`) ||
		!strings.Contains(out, `"data":"`+r90Cipher+`"`) {
		t.Errorf("客户端流里没有涂抹块：\n%s", out)
	}
	if !strings.Contains(out, "visible answer") {
		t.Errorf("正文被误删：\n%s", out)
	}
	if notes := enc.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 自家误报损耗：%v", notes)
	}
}
