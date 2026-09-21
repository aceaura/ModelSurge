package relay

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 「上游给整份 JSON、客户端要 SSE」这条兜底路径上重放签名时必须带上来源。
// 丢了来源等于把上游的真签名降级成来源不明，下游编码器会把它当外族丢弃，
// 而另一条（上游本就流式）路径照样全绿。
func TestEventsFromResponseKeepsSignatureOrigin(t *testing.T) {
	for _, from := range []string{"anthropic", "gemini", ir.SigSynthetic} {
		resp := &ir.Response{
			ID: "msg_1", Model: "m",
			Content: []ir.Block{{Type: ir.BlockThinking, Thinking: &ir.Thinking{
				Text: "想", Signature: "SIGVALUE", SignatureFrom: from,
			}}},
			StopReason: ir.StopEndTurn,
		}
		var sig *ir.Event
		for _, ev := range EventsFromResponse(resp) {
			if ev.Type == ir.EvSigDelta {
				e := ev
				sig = &e
			}
		}
		if sig == nil {
			t.Fatalf("from=%q 没重放签名事件", from)
		}
		if sig.SignatureFrom != from {
			t.Errorf("来源 = %q, want %q", sig.SignatureFrom, from)
		}
	}
}

// 流式路径一整轮往返：签名逐片到达，聚合出的 thinking 必须同时带上签名与来源，
// 否则下一轮请求方向会把自家真签名当外族丢掉，会话签名链每轮都断。
func TestStreamedSignatureSurvivesOneTurn(t *testing.T) {
	agg := ir.NewAggregator()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		{Type: ir.EvThinkingDelta, Index: 0, Text: "想"},
		{Type: ir.EvSigDelta, Index: 0, Text: "sig-", SignatureFrom: "anthropic"},
		{Type: ir.EvSigDelta, Index: 0, Text: "tail"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	} {
		agg.Feed(ev)
	}
	resp, aggErr := agg.Finish()
	if aggErr != nil {
		t.Fatalf("聚合报错：%+v", aggErr)
	}
	var th *ir.Thinking
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking {
			th = b.Thinking
		}
	}
	if th == nil {
		t.Fatalf("聚合结果里没有 thinking 块：%+v", resp.Content)
	}
	if th.Signature != "sig-tail" {
		t.Errorf("签名 = %q, want sig-tail", th.Signature)
	}
	if !th.SignatureGenuineFor("anthropic") {
		t.Errorf("同族真签名被判为不可用：from=%q", th.SignatureFrom)
	}
	if th.SignatureGenuineFor("gemini") {
		t.Error("anthropic 签名在 gemini 通道上被判为可用")
	}
}
