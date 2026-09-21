package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

func thinkingStream() []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		{Type: ir.EvThinkingDelta, Index: 0, Text: "secret reasoning"},
		{Type: ir.EvSigDelta, Index: 0, Text: "sig-bytes"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 1, Text: "visible answer"},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 5}},
		{Type: ir.EvMessageStop},
	}
}

func encodeStream(t *testing.T, c proto.InboundCodec, events []ir.Event) string {
	t.Helper()
	enc := c.NewStreamEncoder()
	var sb strings.Builder
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%v): %v", ev.Type, err)
		}
		for _, fr := range frames {
			sb.Write(fr)
		}
	}
	for _, fr := range enc.Finish() {
		sb.Write(fr)
	}
	return sb.String()
}

func hideReq() *ir.Request {
	return &ir.Request{Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096, HideThoughts: true}}
}

// 抑制对四个入站协议都要生效：装饰在 InboundCodec 上，与具体协议无关。
func TestHideThoughtsStripsThinkingForAllInbound(t *testing.T) {
	for _, name := range []string{"anthropic", "gemini", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			base := proto.MustInbound(name)
			plain := encodeStream(t, base, thinkingStream())
			if !strings.Contains(plain, "secret reasoning") {
				t.Fatalf("夹具无效：未抑制时本应含思考文本\n%s", plain)
			}
			hidden := encodeStream(t, withHiddenThoughts(base, hideReq()), thinkingStream())
			if strings.Contains(hidden, "secret reasoning") {
				t.Errorf("思考文本泄漏：\n%s", hidden)
			}
			if strings.Contains(hidden, "sig-bytes") {
				t.Errorf("思考签名泄漏：\n%s", hidden)
			}
			if !strings.Contains(hidden, "visible answer") {
				t.Errorf("正文被误删：\n%s", hidden)
			}
		})
	}
}

// 客户端没表态时必须一字不改：零值即「照常回显」。
func TestHideThoughtsNoopWhenNotRequested(t *testing.T) {
	base := proto.MustInbound("anthropic")
	for _, req := range []*ir.Request{
		nil,
		{},
		{Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096}},
	} {
		if got := withHiddenThoughts(base, req); got != base {
			t.Errorf("未要求隐藏时不应包装：req=%+v", req)
		}
	}
}

// 非流式方向同样要抑制，且不得改动调用方持有的聚合响应
// （估算 usage 与日志摘要还在读它）。
func TestHideThoughtsStripsNonStreamingWithoutMutating(t *testing.T) {
	resp := &ir.Response{
		ID: "msg", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "secret reasoning", Signature: "sig-bytes"}},
			{Type: ir.BlockText, Text: "visible answer"},
		},
	}
	c := withHiddenThoughts(proto.MustInbound("anthropic"), hideReq())
	body, err := c.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if strings.Contains(string(body), "secret reasoning") || strings.Contains(string(body), "sig-bytes") {
		t.Errorf("非流式响应泄漏思考：%s", body)
	}
	if !strings.Contains(string(body), "visible answer") {
		t.Errorf("非流式正文被误删：%s", body)
	}
	if len(resp.Content) != 2 || resp.Content[0].Type != ir.BlockThinking {
		t.Errorf("原响应被就地改坏：%+v", resp.Content)
	}
}

// 没有思考块时 stripThinking 必须返回同一对象（不做无谓拷贝）。
func TestStripThinkingReturnsSameWhenNothingToStrip(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}
	if got := stripThinking(resp); got != resp {
		t.Error("无思考块时不应拷贝")
	}
	if stripThinking(nil) != nil {
		t.Error("nil 应原样返回")
	}
}

// 思考块的 start/stop 必须一并吞掉：只滤 delta 会给下游留一个空块，
// Anthropic 客户端会因缺 signature 报错。
func TestHideThoughtsDropsBlockFraming(t *testing.T) {
	base := proto.MustInbound("anthropic")
	out := encodeStream(t, withHiddenThoughts(base, hideReq()), thinkingStream())
	if strings.Contains(out, `"type":"thinking"`) {
		t.Errorf("思考块外壳未吞掉：\n%s", out)
	}
	// 正文块的 index 1 框架仍要在，且 start/stop 必须配平：
	// 只吞 start 不吞 stop 会给客户端留一个「停止从未开始的块」的事件。
	// 只数 data 行里的 type：SSE 帧的 event: 行也含同一字面量，直接数会翻倍。
	starts := strings.Count(out, `"type":"content_block_start"`)
	stops := strings.Count(out, `"type":"content_block_stop"`)
	if starts != 1 || stops != 1 {
		t.Errorf("块框架应只剩正文一对（start=%d stop=%d）：\n%s", starts, stops, out)
	}
}

// recordEncoder 记录内层收到的事件。四个入站编码器对「未开启块的 stop」
// 都恰好 no-op，所以漏传 stop 在 wire 上看不出来；装饰器的契约是
// 「被抑制块的任何事件都不得到达内层」，只能在这一层断言。
type recordEncoder struct{ got []ir.Event }

func (r *recordEncoder) Encode(ev ir.Event) ([][]byte, error) {
	r.got = append(r.got, ev)
	return nil, nil
}
func (r *recordEncoder) Finish() [][]byte { return nil }

type recordCodec struct {
	proto.InboundCodec
	enc *recordEncoder
}

func (c recordCodec) NewStreamEncoder() proto.StreamEncoder { return c.enc }

func TestHideThoughtsInnerNeverSeesSuppressedBlock(t *testing.T) {
	rec := &recordEncoder{}
	c := withHiddenThoughts(recordCodec{InboundCodec: proto.MustInbound("anthropic"), enc: rec}, hideReq())
	encodeStream(t, c, thinkingStream())
	blockScoped := map[ir.EventType]bool{
		ir.EvBlockStart: true, ir.EvBlockStop: true,
		ir.EvTextDelta: true, ir.EvThinkingDelta: true,
		ir.EvSigDelta: true, ir.EvToolInput: true,
	}
	for _, ev := range rec.got {
		// 消息级事件的 Index 恒为零值，与块序号无关，不能一并判。
		if blockScoped[ev.Type] && ev.Index == 0 {
			t.Errorf("被抑制块的事件到达内层：%s idx=%d", ev.Type, ev.Index)
		}
	}
	if len(rec.got) == 0 {
		t.Fatal("夹具无效：内层一个事件都没收到")
	}
	// 未被抑制的块必须原样穿透，序列也要保持：把正文块的 stop 一并吞掉时，
	// 该块只能靠 Finish 收尾，content_block_stop 会排到 message_stop 之后。
	var seq []ir.EventType
	for _, ev := range rec.got {
		seq = append(seq, ev.Type)
	}
	want := []ir.EventType{ir.EvMessageStart, ir.EvBlockStart, ir.EvTextDelta, ir.EvBlockStop, ir.EvMessageDelta, ir.EvMessageStop}
	if len(seq) != len(want) {
		t.Fatalf("内层事件序列 = %v，want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("内层事件序列 = %v，want %v", seq, want)
		}
	}
}

// Finish 必须转交给内层：思考被抑制不影响流的收尾。
// 未收到 message_stop 时内层要补终止事件，装饰器吞掉就会让客户端流悬挂。
func TestHideThoughtsForwardsFinish(t *testing.T) {
	truncated := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		{Type: ir.EvThinkingDelta, Index: 0, Text: "secret reasoning"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 1, Text: "visible answer"},
		// 上游断流：没有 block_stop / message_delta / message_stop
	}
	out := encodeStream(t, withHiddenThoughts(proto.MustInbound("anthropic"), hideReq()), truncated)
	if !strings.Contains(out, "message_stop") {
		t.Errorf("Finish 未转交，流没有收尾：\n%s", out)
	}
}

// 端到端：装配点在 Forward 入口，五处写出点靠装饰器覆盖。
// 只测装饰器本身会漏掉「入口忘了包」这一类遗漏。
func TestForwardHidesThoughtsEndToEnd(t *testing.T) {
	var nd bytes.Buffer
	for _, ev := range thinkingStream() {
		_ = json.NewEncoder(&nd).Encode(ev)
	}
	replay := &kiroExecuteReplay{lease: kiroLease(), body: io.NopCloser(bytes.NewReader(nd.Bytes()))}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("gemini"), &ir.Request{
		Model:    "claude-sonnet-5",
		Stream:   true,
		Thinking: &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096, HideThoughts: true},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	body := w.Body.String()
	if strings.Contains(body, "secret reasoning") || strings.Contains(body, "sig-bytes") {
		t.Errorf("Forward 未接入抑制，思考泄漏到客户端：\n%s", body)
	}
	if !strings.Contains(body, "visible answer") {
		t.Errorf("正文未送达：\n%s", body)
	}
}

// 只发思考、没有正文时也不能崩，且输出里不含思考。
func TestHideThoughtsThinkingOnlyStream(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		{Type: ir.EvThinkingDelta, Index: 0, Text: "secret reasoning"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	}
	out := encodeStream(t, withHiddenThoughts(proto.MustInbound("anthropic"), hideReq()), events)
	if strings.Contains(out, "secret reasoning") {
		t.Errorf("思考文本泄漏：\n%s", out)
	}
}
