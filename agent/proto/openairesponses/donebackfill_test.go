package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// done 事件携带的是完整终态值而不是新一份内容。只发终态不发增量的上游
// （done-only 网关）整段正文只在这一帧出现，此前整个事件落到 default 被丢掉，
// 客户端一个字都收不到。判据与 R73 的工具参数一致：只补尚未发出的后缀。

const r78Created = `{"type":"response.created","response":{"id":"r1","model":"gpt"}}`

const r78MessageAdded = `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1","role":"assistant"}}`

// done-only：三处终态帧都带全文，正文必须落地且只落地一次。
func TestStreamDecodeDoneOnlyTextBackfillsOnce(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		r78MessageAdded,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"整段正文"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"整段正文"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"整段正文","annotations":[]}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	order, _, text := blocksOf(evs)
	if len(order) != 1 {
		t.Fatalf("开块数 = %d（%v），want 1：终态帧各自开了一块", len(order), order)
	}
	if text[order[0]] != "整段正文" {
		t.Fatalf("正文 = %q, want %q", text[order[0]], "整段正文")
	}
}

// 连 content_part.added 都没有、只有 output_item.done 带 content：仍要出正文。
func TestStreamDecodeItemDoneOnlyBackfillsFromContent(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"只有终态","annotations":[]}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	order, typ, text := blocksOf(evs)
	if len(order) != 1 || typ[order[0]] != ir.BlockText {
		t.Fatalf("块 = %v/%v, want 一个 text 块", order, typ)
	}
	if text[order[0]] != "只有终态" {
		t.Fatalf("正文 = %q, want %q", text[order[0]], "只有终态")
	}
}

// 增量只来了一半：done 只补缺失后缀，不得把已有前缀再发一遍。
func TestStreamDecodePartialDeltaThenDoneAppendsOnlySuffix(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"前半"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"前半后半"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"前半后半"}}`,
	)
	_, _, text := blocksOf(evs)
	if text[0] != "前半后半" {
		t.Fatalf("正文 = %q, want %q（重复前缀或丢后缀都算失败）", text[0], "前半后半")
	}
}

// 增量已给全：done 不得再产出任何增量。
func TestStreamDecodeFullDeltaThenDoneDoesNotDuplicate(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"完整"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"完整"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"完整"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"完整"}]}}`,
	)
	n := 0
	for _, ev := range evs {
		if ev.Type == ir.EvTextDelta {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("文本增量事件数 = %d, want 1（done 被当成了新一份内容）", n)
	}
	if _, _, text := blocksOf(evs); text[0] != "完整" {
		t.Fatalf("正文 = %q, want %q", text[0], "完整")
	}
}

// 终态与增量分叉（上游自己不一致）：保留已下发的增量，不追加成畸形正文。
func TestStreamDecodeDivergingDoneIsIgnored(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ABC"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"XYZ"}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "ABC" {
		t.Fatalf("正文 = %q, want 保留已下发的 %q", text[0], "ABC")
	}
}

// 拒绝正文的终态在 refusal.done / content_part.done / output_item.done 里各出现
// 一次，且寻址键带 refusal 位：漏了就会与流式那块错开，同一段拒绝下发两遍。
func TestStreamDecodeRefusalDoneBackfillsOwnBlockOnce(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		r78MessageAdded,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"我不能这么做"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"我不能这么做"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"refusal","refusal":"我不能这么做"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	order, typ, text := blocksOf(evs)
	if len(order) != 1 || typ[order[0]] != ir.BlockRefusal {
		t.Fatalf("块 = %v/%v, want 单个 refusal 块", order, typ)
	}
	if text[order[0]] != "我不能这么做" {
		t.Fatalf("拒绝正文 = %q, want %q", text[order[0]], "我不能这么做")
	}
}

// 思考正文的终态在 reasoning_summary_text.done，签名在 output_item.done。
// 正文增量必须排在 signature_delta 之前，且 summary 快照不得再补一遍。
func TestStreamDecodeReasoningDoneBackfillsBeforeSignature(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"思考全文"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"思考全文"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"思考全文"}],"encrypted_content":"SIG"}}`,
	)
	think, sig := -1, -1
	for n, ev := range evs {
		switch ev.Type {
		case ir.EvThinkingDelta:
			if think >= 0 {
				t.Fatalf("思考增量被下发了两次：%v", types(evs))
			}
			think = n
			if ev.Text != "思考全文" {
				t.Fatalf("思考正文 = %q, want %q", ev.Text, "思考全文")
			}
		case ir.EvSigDelta:
			sig = n
		}
	}
	if think < 0 || sig < 0 {
		t.Fatalf("事件序列缺项 think=%d sig=%d：%v", think, sig, types(evs))
	}
	if think > sig {
		t.Fatalf("思考正文落在了 signature_delta 之后：%v", types(evs))
	}
}

// reasoning_summary_part.done 也要能单独把思考正文补齐（有的网关只发这一帧）。
func TestStreamDecodeReasoningSummaryPartDoneBackfills(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"只有part终态"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "只有part终态" {
		t.Fatalf("思考正文 = %q, want %q", text[0], "只有part终态")
	}
}

// done 里的 annotations 是全量快照：annotation.added 已给过的不得重复下发。
func TestStreamDecodeAnnotationSnapshotDedupesAgainstAdded(t *testing.T) {
	frames := []string{
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"北京晴。"}`,
	}
	const ann = `{"type":"url_citation","url":"https://w","title":"T","start_index":0,"end_index":4}`
	withAdded := append(append([]string{}, frames...),
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":0,"annotation":`+ann+`}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"北京晴。","annotations":[`+ann+`]}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"北京晴。","annotations":[`+ann+`]}}`,
	)
	if got := countCitations(feedFrames(t, withAdded...)); got != 1 {
		t.Fatalf("引用条数 = %d, want 1（快照与增量重复计数）", got)
	}
	// 反向：只有快照、没有 annotation.added，引用仍要落地。
	withoutAdded := append(append([]string{}, frames...),
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"北京晴。","annotations":[`+ann+`]}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"北京晴。","annotations":[`+ann+`]}}`,
	)
	if got := countCitations(feedFrames(t, withoutAdded...)); got != 1 {
		t.Fatalf("无增量时引用条数 = %d, want 1（快照被当成重复丢掉了）", got)
	}
}

func countCitations(evs []ir.Event) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == ir.EvCitation {
			n += len(ev.Citations)
		}
	}
	return n
}

// 多 part 的 done-only 流：每个 part 的终态正文各归各块，content_index 就是
// item.content 里的位置。
func TestStreamDecodeMultiPartDoneOnlyBackfillsPerPart(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[`+
			`{"type":"output_text","text":"第一段"},{"type":"output_text","text":"第二段"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	order, _, text := blocksOf(evs)
	if len(order) != 2 {
		t.Fatalf("开块数 = %d, want 2（多 part 被并成一块）", len(order))
	}
	if text[order[0]] != "第一段" || text[order[1]] != "第二段" {
		t.Fatalf("正文 = %q/%q, want 第一段/第二段", text[order[0]], text[order[1]])
	}
}

// 块已关就不再回补：终态比已下发内容更长时，追加会落到已定稿的 item 上
// （对齐 new-api mergeFinalValue 的 block.Stopped 判据）。
func TestStreamDecodeBackfillAfterCloseIsIgnored(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"A"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"AB"}}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ABC"}`,
	)
	order, _, text := blocksOf(evs)
	if len(order) != 1 {
		t.Fatalf("开块数 = %d, want 1", len(order))
	}
	if text[0] != "AB" {
		t.Fatalf("正文 = %q, want %q（关块后又被追加）", text[0], "AB")
	}
}

// 编码侧：part 级终止帧必须齐全且顺序为 *.done -> content_part.done ->
// output_item.done。只发 output_item.done 的话，按 part 事件关块的下游
// （cc-switch 把 output_text.done 映射成 content_block_stop）永远等不到块结束。
func TestStreamEncodeEmitsPartDoneFramesInOfficialOrder(t *testing.T) {
	s := encodeStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gpt"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "正文"},
		ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{URL: "https://w", Title: "T", Start: 0, End: 2}}},
		ir.Event{Type: ir.EvBlockStop, Index: 0},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	)
	textDone := strings.Index(s, `"response.output_text.done"`)
	partDone := strings.Index(s, `"response.content_part.done"`)
	itemDone := strings.Index(s, `"response.output_item.done"`)
	if textDone < 0 || partDone < 0 || itemDone < 0 {
		t.Fatalf("缺 part 级终止帧：text.done=%d part.done=%d item.done=%d", textDone, partDone, itemDone)
	}
	if !(textDone < partDone && partDone < itemDone) {
		t.Fatalf("终止帧顺序不对：%d/%d/%d", textDone, partDone, itemDone)
	}
	// 完整正文与全量引用都要在 done 帧里，否则只读终态的下游拿不到内容。
	done := s[textDone:partDone]
	if !strings.Contains(done, `"text":"正文"`) || !strings.Contains(done, `"url":"https://w"`) {
		t.Fatalf("output_text.done 没带完整终态：%s", done)
	}
	part := s[partDone:itemDone]
	if !strings.Contains(part, `"part":{"type":"output_text","text":"正文","annotations":[{"type":"url_citation","url":"https://w","title":"T","start_index":0,"end_index":2}]}`) {
		t.Fatalf("content_part.done 没带完整终态：%s", part)
	}
}

// 拒绝块的终态用 refusal 字段而不是 text：写错字段下游读不到拒绝正文。
func TestStreamEncodeRefusalDoneUsesRefusalField(t *testing.T) {
	s := encodeStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gpt"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockRefusal}},
		ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "不行"},
		ir.Event{Type: ir.EvBlockStop, Index: 0},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopRefusal},
	)
	if !strings.Contains(s, `"response.refusal.done","output_index":0,"content_index":0,"refusal":"不行"`) {
		t.Fatalf("refusal.done 帧不对：\n%s", s)
	}
	if !strings.Contains(s, `"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"不行"}`) {
		t.Fatalf("content_part.done 帧不对：\n%s", s)
	}
	if strings.Contains(s, `"response.output_text.done"`) {
		t.Fatalf("拒绝块发成了 output_text.done：\n%s", s)
	}
}

// 思考块的终态是 reasoning_summary_text.done + reasoning_summary_part.done。
func TestStreamEncodeReasoningDoneFrames(t *testing.T) {
	s := encodeStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gpt"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		ir.Event{Type: ir.EvThinkingDelta, Index: 0, Text: "想"},
		ir.Event{Type: ir.EvBlockStop, Index: 0},
		ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
	)
	sumDone := strings.Index(s, `"response.reasoning_summary_text.done"`)
	partDone := strings.Index(s, `"response.reasoning_summary_part.done"`)
	itemDone := strings.Index(s, `"response.output_item.done"`)
	if sumDone < 0 || partDone < 0 || !(sumDone < partDone && partDone < itemDone) {
		t.Fatalf("思考终止帧缺失或顺序不对：%d/%d/%d", sumDone, partDone, itemDone)
	}
	if !strings.Contains(s[sumDone:partDone], `"text":"想"`) {
		t.Fatalf("reasoning_summary_text.done 没带完整思考：%s", s[sumDone:partDone])
	}
}

// 端到端：done-only 上游 -> IR -> Responses 客户端，正文只出现一次。
func TestResponsesRoundTripDoneOnlyKeepsTextOnce(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"整段正文"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"整段正文"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"整段正文"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	s := encodeStream(t, evs...)
	// 增量 + output_text.done + content_part.done + output_item.done + completed.output
	if got := strings.Count(s, "整段正文"); got != 5 {
		t.Fatalf("正文出现 %d 次, want 5（一份增量四份终态）：\n%s", got, s)
	}
	if strings.Count(s, `"response.output_text.delta"`) != 1 {
		t.Fatalf("回补的正文没有以增量形式下发给客户端：\n%s", s)
	}
}

// 端到端：正常全增量流往返后正文不得翻倍。
func TestResponsesRoundTripNormalStreamNoDuplication(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"完整"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"完整"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"完整"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"完整"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	s := encodeStream(t, evs...)
	if strings.Contains(s, "完整完整") {
		t.Fatalf("正文被翻倍：\n%s", s)
	}
	if strings.Count(s, `"response.output_text.delta"`) != 1 {
		t.Fatalf("增量帧数不对：\n%s", s)
	}
}

// ---- 单一来源隔离 ----
// 下面每个用例只让一处终态帧携带内容，其余终态帧缺席或为空。
// 少了这组隔离，任一回补点被摘掉都能由别的点兜住，变异检不出来。

func TestStreamDecodeOutputTextDoneIsSoleTextSource(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"甲"}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "甲" {
		t.Fatalf("正文 = %q, want %q", text[0], "甲")
	}
}

func TestStreamDecodeContentPartDoneIsSoleTextSource(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"乙"}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "乙" {
		t.Fatalf("正文 = %q, want %q", text[0], "乙")
	}
}

func TestStreamDecodeRefusalDoneIsSoleTextSource(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"丙"}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	order, typ, text := blocksOf(evs)
	if len(order) != 1 || typ[order[0]] != ir.BlockRefusal {
		t.Fatalf("块 = %v/%v, want 单个 refusal 块", order, typ)
	}
	if text[order[0]] != "丙" {
		t.Fatalf("拒绝正文 = %q, want %q", text[order[0]], "丙")
	}
}

func TestStreamDecodeReasoningSummaryTextDoneIsSoleSource(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"丁"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "丁" {
		t.Fatalf("思考正文 = %q, want %q", text[0], "丁")
	}
}

// reasoning_text.done 是原始推理文本的终态，与 summary 走同一条回补路径。
func TestStreamDecodeReasoningTextDoneIsSoleSource(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_text.done","output_index":0,"text":"原始推理"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "原始推理" {
		t.Fatalf("思考正文 = %q, want %q", text[0], "原始推理")
	}
}

// summary 快照只在 output_item.done 里（有的网关不发 reasoning_* 终止帧），
// 且签名必须照常下发。
func TestStreamDecodeItemDoneReasoningSummaryIsSoleSource(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"戊"}],"encrypted_content":"SIG"}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "戊" {
		t.Fatalf("思考正文 = %q, want %q", text[0], "戊")
	}
	var sig int
	for _, ev := range evs {
		if ev.Type == ir.EvSigDelta {
			sig++
		}
	}
	if sig != 1 {
		t.Fatalf("signature_delta 数 = %d, want 1", sig)
	}
}

const r78Ann = `{"type":"url_citation","url":"https://w","title":"T","start_index":0,"end_index":4}`
const r78Ann2 = `{"type":"url_citation","url":"https://x","title":"U","start_index":4,"end_index":8}`

func TestStreamDecodeAnnotationSnapshotOnlyInOutputTextDone(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"北京晴。"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"北京晴。","annotations":[`+r78Ann+`]}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if got := countCitations(evs); got != 1 {
		t.Fatalf("引用条数 = %d, want 1", got)
	}
}

func TestStreamDecodeAnnotationSnapshotOnlyInContentPartDone(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"北京晴。"}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"北京晴。","annotations":[`+r78Ann+`]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if got := countCitations(evs); got != 1 {
		t.Fatalf("引用条数 = %d, want 1", got)
	}
}

// 快照会长大：后一帧比前一帧多一条时只补新增的那条，不重发已有的。
func TestStreamDecodeGrowingAnnotationSnapshotEmitsOnlyNew(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"北京晴。明天有雨。"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"北京晴。明天有雨。","annotations":[`+r78Ann+`]}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"北京晴。明天有雨。","annotations":[`+r78Ann+`,`+r78Ann2+`]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if got := countCitations(evs); got != 2 {
		t.Fatalf("引用条数 = %d, want 2（快照被整份重发）", got)
	}
	var urls []string
	for _, ev := range evs {
		for _, c := range ev.Citations {
			urls = append(urls, c.URL)
		}
	}
	if strings.Join(urls, ",") != "https://w,https://x" {
		t.Fatalf("引用顺序/内容 = %v, want [https://w https://x]", urls)
	}
}

// 拒绝与思考的增量也要记账，否则终态回补会把已有前缀再发一遍。
func TestStreamDecodeRefusalPartialDeltaThenDoneAppendsOnlySuffix(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"我不能"}`,
		`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"我不能这么做"}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "我不能这么做" {
		t.Fatalf("拒绝正文 = %q, want %q", text[0], "我不能这么做")
	}
}

func TestStreamDecodeReasoningPartialDeltaThenDoneAppendsOnlySuffix(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"先想"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"先想后说"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"先想后说"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if _, _, text := blocksOf(evs); text[0] != "先想后说" {
		t.Fatalf("思考正文 = %q, want %q", text[0], "先想后说")
	}
}

// 后到的快照比先前的更短（上游自相矛盾）：不得重发，更不得因切片越界 panic。
// 引用计数守卫挡的就是这一条。
func TestStreamDecodeShrinkingAnnotationSnapshotIsSafe(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"北京晴。明天有雨。"}`,
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":0,"annotation":`+r78Ann+`}`,
		`{"type":"response.output_text.annotation.added","output_index":0,"content_index":0,"annotation":`+r78Ann2+`}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"北京晴。明天有雨。","annotations":[`+r78Ann+`]}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"北京晴。明天有雨。","annotations":[]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if got := countCitations(evs); got != 2 {
		t.Fatalf("引用条数 = %d, want 2（缩短的快照被重发）", got)
	}
}

// content_part.done 是拒绝正文的唯一来源（网关漏发 refusal.done）。
func TestStreamDecodeContentPartDoneIsSoleRefusalSource(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"丁拒"}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	order, typ, text := blocksOf(evs)
	if len(order) != 1 || typ[order[0]] != ir.BlockRefusal {
		t.Fatalf("块 = %v/%v, want 单个 refusal 块", order, typ)
	}
	if text[order[0]] != "丁拒" {
		t.Fatalf("拒绝正文 = %q, want %q", text[order[0]], "丁拒")
	}
}

// output_item.done 的 content.annotations 是引用的唯一来源。
func TestStreamDecodeAnnotationSnapshotOnlyInItemDone(t *testing.T) {
	evs := feedFrames(t,
		r78Created,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"北京晴。"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"北京晴。","annotations":[`+r78Ann+`]}]}}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	)
	if got := countCitations(evs); got != 1 {
		t.Fatalf("引用条数 = %d, want 1", got)
	}
	if _, _, text := blocksOf(evs); text[0] != "北京晴。" {
		t.Fatalf("正文 = %q, want %q", text[0], "北京晴。")
	}
}
