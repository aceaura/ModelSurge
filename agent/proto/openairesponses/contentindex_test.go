package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// feedFrames 把一串 wire 帧喂给解码器，收齐 IR 事件。
func feedFrames(t *testing.T, frames ...string) []ir.Event {
	t.Helper()
	dec := codec{}.NewStreamDecoder()
	var evs []ir.Event
	for _, f := range frames {
		got, err := dec.Feed("", f)
		if err != nil {
			t.Fatalf("Feed(%s): %v", f, err)
		}
		evs = append(evs, got...)
	}
	return append(evs, dec.Finish()...)
}

// blocksOf 按开块顺序返回 (IR 序号 -> 块类型) 与该块累积的正文。
func blocksOf(evs []ir.Event) (order []int, typ map[int]ir.BlockType, text map[int]string) {
	typ, text = map[int]ir.BlockType{}, map[int]string{}
	for _, ev := range evs {
		switch ev.Type {
		case ir.EvBlockStart:
			if _, dup := typ[ev.Index]; dup {
				continue
			}
			order = append(order, ev.Index)
			typ[ev.Index] = ev.Block.Type
		case ir.EvTextDelta, ir.EvThinkingDelta, ir.EvToolInput:
			text[ev.Index] += ev.Text
		}
	}
	return order, typ, text
}

// 一条 message 带多个 content part 时，每个 part 必须各占一块。并进同一块会让
// 引用索引跨 part 错位（annotation 的 start_index 是相对本 part 正文的）。
func TestStreamDecodeMultiContentPartsGetDistinctBlocks(t *testing.T) {
	evs := feedFrames(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"A"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"A"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"B"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":1,"part":{"type":"output_text","text":"B"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
	)
	order, typ, text := blocksOf(evs)
	if len(order) != 2 {
		t.Fatalf("两个 content part 被并成 %d 块：%+v", len(order), text)
	}
	for _, i := range order {
		if typ[i] != ir.BlockText {
			t.Errorf("块 %d 类型 = %v, want text", i, typ[i])
		}
	}
	if text[order[0]] != "A" || text[order[1]] != "B" {
		t.Errorf("正文串块了：%q / %q，want A / B", text[order[0]], text[order[1]])
	}
	// 关块顺序必须与开块顺序一致：交叉嵌套会让严格按块配对的客户端错乱。
	var stops []int
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStop {
			stops = append(stops, ev.Index)
		}
	}
	if len(stops) != 2 || stops[0] != order[0] || stops[1] != order[1] {
		t.Errorf("关块顺序 = %v, want %v", stops, order)
	}
}

// 引用必须落在自己那个 part 上，索引原样保留。此前两个 part 被并成 "AB"，
// content_index=1 上的 start_index=0 就被读成了指向 "A"。
func TestStreamDecodeAnnotationAttachesToOwnPart(t *testing.T) {
	evs := feedFrames(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"A"}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"B"}`,
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":1,
			"annotation":{"type":"url_citation","url":"https://w","title":"T","start_index":0,"end_index":1}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
	)
	_, _, text := blocksOf(evs)
	var cite *ir.Event
	for i := range evs {
		if evs[i].Type == ir.EvCitation {
			cite = &evs[i]
		}
	}
	if cite == nil {
		t.Fatalf("引用整条丢失：%+v", evs)
	}
	if text[cite.Index] != "B" {
		t.Fatalf("引用落在了正文为 %q 的块上，want %q（part 错位）", text[cite.Index], "B")
	}
	if got := ir.ResolveCitedText(text[cite.Index], cite.Citations[0]); got != "B" {
		t.Errorf("按索引切出的引用正文 = %q, want %q", got, "B")
	}
}

// 纯拒绝消息不得多出空文本块：output_item.added 只说明这是个 message，
// 类型要到 content_part.added 才知道。提前开 text 块会让客户端多渲染一条空回答。
func TestStreamDecodeRefusalOnlyMessageHasNoEmptyTextBlock(t *testing.T) {
	evs := feedFrames(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"不行"}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"不行"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"refusal","refusal":"不行"}]}}`,
	)
	order, typ, text := blocksOf(evs)
	if len(order) != 1 {
		t.Fatalf("纯拒绝消息开出了 %d 块：%+v", len(order), text)
	}
	if typ[order[0]] != ir.BlockRefusal {
		t.Errorf("块类型 = %v, want refusal", typ[order[0]])
	}
	if text[order[0]] != "不行" {
		t.Errorf("拒绝正文 = %q", text[order[0]])
	}
}

// 同一条 message 里文本与拒绝共存时也不能并块——上游漏发 content_part.added、
// 两个 part 共用 (output_index, content_index) 时尤其如此。
func TestStreamDecodeTextAndRefusalNeverShareBlock(t *testing.T) {
	evs := feedFrames(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"前半"}`,
		`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"后半拒绝"}`,
	)
	order, typ, text := blocksOf(evs)
	if len(order) != 2 {
		t.Fatalf("文本与拒绝并成了 %d 块：%+v", len(order), text)
	}
	var refIdx, txtIdx = -1, -1
	for _, i := range order {
		switch typ[i] {
		case ir.BlockRefusal:
			refIdx = i
		case ir.BlockText:
			txtIdx = i
		}
	}
	if refIdx == txtIdx || refIdx < 0 {
		t.Fatalf("没有独立的拒绝块：%+v", typ)
	}
	if text[refIdx] != "后半拒绝" || text[txtIdx] != "前半" {
		t.Errorf("正文串块：refusal=%q text=%q", text[refIdx], text[txtIdx])
	}
}

// content_part.done 只关自己那一块，不能顺手把同 item 的其它 part 一起关掉。
func TestStreamDecodeContentPartDoneClosesOnlyOwnPart(t *testing.T) {
	evs := feedFrames(t,
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text"}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"A"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"A"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text"}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"B"}`,
	)
	order, _, _ := blocksOf(evs)
	if len(order) != 2 {
		t.Fatalf("块数 = %d, want 2", len(order))
	}
	// part0 必须在 part1 开块之前就关掉。全部延后到 output_item.done 会让下游
	// 看到 start/start/stop/stop 的交叉嵌套，严格按块配对的客户端会错乱。
	stop0, start1 := -1, -1
	for i, ev := range evs {
		switch {
		case ev.Type == ir.EvBlockStop && ev.Index == order[0] && stop0 < 0:
			stop0 = i
		case ev.Type == ir.EvBlockStart && ev.Index == order[1]:
			start1 = i
		}
	}
	if stop0 < 0 || start1 < 0 || stop0 > start1 {
		t.Fatalf("关块位置 = %d, 第二块开块位置 = %d，want 前者更早（事件序 %+v）", stop0, start1, types(evs))
	}
}

// types 事件类型序列，失败信息里用来看形状。
func types(evs []ir.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, string(ev.Type))
	}
	return out
}

// 工具参数状态按 IR 块序号记账：两个 output_index 的调用不得互相串参数。
func TestStreamDecodeToolArgsTrackedPerBlock(t *testing.T) {
	evs := feedFrames(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"a"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c2","name":"b"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"x\":1}"}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"y\":2}"}`,
		`{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"y\":2}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"x\":1}"}`,
	)
	_, _, text := blocksOf(evs)
	var got []string
	for _, v := range []string{text[0], text[1]} {
		got = append(got, v)
	}
	if text[0] != `{"x":1}` || text[1] != `{"y":2}` {
		t.Fatalf("工具参数串块：%v", got)
	}
}

// encodeStream 把 IR 事件编成 Responses SSE 全文。
func encodeStream(t *testing.T, evs ...ir.Event) string {
	t.Helper()
	enc := codec{}.NewStreamEncoder()
	var sb strings.Builder
	for _, ev := range evs {
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

// IR 序号是内部编号，可能稀疏（别族解码器给的、或历史遗留的偏移区段）。
// 直接写进 wire 会让客户端按 output_index 去索引 response.output[] 时越界。
func TestStreamEncodeRemapsSparseIRIndexToDenseOutputIndex(t *testing.T) {
	s := encodeStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "正文"},
		ir.Event{Type: ir.EvBlockStop, Index: 0},
		ir.Event{Type: ir.EvBlockStart, Index: 1 << 20, Block: &ir.Block{Type: ir.BlockRefusal}},
		ir.Event{Type: ir.EvTextDelta, Index: 1 << 20, Text: "拒绝"},
		ir.Event{Type: ir.EvBlockStop, Index: 1 << 20},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	)
	if strings.Contains(s, "1048576") {
		t.Errorf("内部 IR 序号泄漏到 wire：\n%s", s)
	}
	for _, want := range []string{
		`"response.refusal.delta","output_index":1`,
		`"response.output_item.done","output_index":1`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wire 缺 %s：\n%s", want, s)
		}
	}
	// 终止帧的 output[] 必须与 output_index 同序，否则客户端按索引取到别的 item。
	var final struct {
		Response struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	line := ""
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, `"response.completed"`) {
			line = strings.TrimPrefix(l, "data: ")
		}
	}
	if line == "" {
		t.Fatalf("没有终止帧：\n%s", s)
	}
	if err := json.Unmarshal([]byte(line), &final); err != nil {
		t.Fatalf("解析终止帧: %v\n%s", err, line)
	}
	out := final.Response.Output
	if len(out) != 2 || out[0].Content[0].Type != "output_text" || out[1].Content[0].Type != "refusal" {
		t.Fatalf("output[] 顺序与 output_index 不一致：%+v", out)
	}
}

// output_index / content_index 是必填字段，0 也是合法值。omitempty 会把它们
// 整个抹掉，官方 SDK 读到 undefined 就索引不了 response.output[]。
func TestStreamEncodeEmitsZeroValuedIndices(t *testing.T) {
	s := encodeStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{URL: "https://w", Start: 0, End: 2}}},
		ir.Event{Type: ir.EvBlockStop, Index: 0},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	)
	for _, want := range []string{
		`"response.output_item.added","output_index":0`,
		`"response.content_part.added","output_index":0,"content_index":0`,
		`"response.output_text.delta","output_index":0,"content_index":0`,
		`"response.output_text.annotation.added","output_index":0,"content_index":0`,
		`"response.output_item.done","output_index":0`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wire 缺 %s：\n%s", want, s)
		}
	}
	// 不携带块索引的事件不得凭空多出 output_index。
	if strings.Contains(s, `"response.created","output_index"`) {
		t.Errorf("response.created 多带了索引：\n%s", s)
	}
}

// 端到端：多 part + 引用的 Responses 流往返后，引用仍指向正确的那一段正文。
func TestResponsesRoundTripKeepsCitationOnOwnPart(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	enc := codec{}.NewStreamEncoder()
	var sb strings.Builder
	run := func(evs []ir.Event) {
		for _, ev := range evs {
			frames, err := enc.Encode(ev)
			if err != nil {
				t.Fatal(err)
			}
			for _, fr := range frames {
				sb.Write(fr)
			}
		}
	}
	for _, f := range []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"北京晴。"}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"明天有雨。"}`,
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":1,
			"annotation":{"type":"url_citation","url":"https://w","title":"T","start_index":0,"end_index":4}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt","status":"completed"}}`,
	} {
		evs, err := dec.Feed("", f)
		if err != nil {
			t.Fatal(err)
		}
		run(evs)
	}
	run(dec.Finish())
	s := sb.String()
	if strings.Contains(s, "北京晴。明天有雨。") {
		t.Errorf("两个 part 又被并成一段正文：\n%s", s)
	}
	done := s[strings.LastIndex(s, `"response.output_item.done"`):]
	// 第二个 item 的正文是「明天有雨。」，引用 [0,4) 必须落在这段上。
	if !strings.Contains(done, `"text":"明天有雨。"`) {
		t.Fatalf("第二段正文没独立成 item：\n%s", done)
	}
	if !strings.Contains(done, `"start_index":0,"end_index":4`) || !strings.Contains(done, `"url":"https://w"`) {
		t.Errorf("引用没跟着第二段走：\n%s", done)
	}
}

// 端到端：纯拒绝消息往返后只应产出一个 message item，且是 refusal。
func TestResponsesRoundTripRefusalHasNoEmptyMessage(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	enc := codec{}.NewStreamEncoder()
	var sb strings.Builder
	for _, f := range []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal"}}`,
		`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"不行"}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"不行"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt","status":"completed"}}`,
	} {
		evs, err := dec.Feed("", f)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			frames, err := enc.Encode(ev)
			if err != nil {
				t.Fatal(err)
			}
			for _, fr := range frames {
				sb.Write(fr)
			}
		}
	}
	s := sb.String()
	if n := strings.Count(s, `"response.output_item.added"`); n != 1 {
		t.Errorf("产出了 %d 个 output item，want 1（多出来的是空文本消息）：\n%s", n, s)
	}
	if strings.Contains(s, `"type":"output_text"`) {
		t.Errorf("纯拒绝消息里混进了 output_text：\n%s", s)
	}
	if !strings.Contains(s, `"response.refusal.delta","output_index":0,"content_index":0,"delta":"不行"`) {
		t.Errorf("拒绝正文没走 refusal 通道：\n%s", s)
	}
}
