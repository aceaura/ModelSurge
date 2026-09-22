package gemini

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func decodeGrounding(t *testing.T, body []byte) *groundingMetadata {
	t.Helper()
	var r generateResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Candidates) != 1 {
		t.Fatalf("候选数 = %d", len(r.Candidates))
	}
	return r.Candidates[0].GroundingMetadata
}

// 区间口径必须按字节换算：Gemini 与 OpenAI/Anthropic 不同，
// 按字符算会让中文正文的区间整体错位。
func TestEncodeGroundingByteOffsets(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴，明天有雨。",
		Citations: []ir.Citation{{URL: "https://w", Title: "天气", CitedText: "明天有雨"}}}}}
	body, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	gm := decodeGrounding(t, body)
	if gm == nil {
		t.Fatal("没有产出 groundingMetadata")
	}
	if len(gm.GroundingChunks) != 1 || gm.GroundingChunks[0].Web == nil ||
		gm.GroundingChunks[0].Web.URI != "https://w" || gm.GroundingChunks[0].Web.Title != "天气" {
		t.Fatalf("来源清单不对：%+v", gm.GroundingChunks)
	}
	if len(gm.GroundingSupports) != 1 {
		t.Fatalf("support 数 = %d", len(gm.GroundingSupports))
	}
	seg := gm.GroundingSupports[0].Segment
	// "北京今天晴，" 六个字符各 3 字节 = 18；"明天有雨" 四字符 12 字节 => [18,30)
	if seg.StartIndex != 18 || seg.EndIndex != 30 {
		t.Fatalf("segment = [%d,%d)，want [18,30)（字节口径）", seg.StartIndex, seg.EndIndex)
	}
	if seg.Text != "明天有雨" {
		t.Errorf("segment.text = %q", seg.Text)
	}
	if idx := gm.GroundingSupports[0].GroundingChunkIndices; len(idx) != 1 || idx[0] != 0 {
		t.Errorf("chunk 下标不对：%v", idx)
	}
}

// 指向同一来源的多段正文共用一个 chunk：chunk 数组就是来源清单，
// 重复写会让客户端看到重复来源。
func TestEncodeGroundingSharesChunkPerURL(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "aaa bbb",
		Citations: []ir.Citation{
			{URL: "https://w", CitedText: "aaa"},
			{URL: "https://w", CitedText: "bbb"},
		}}}}
	body, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	gm := decodeGrounding(t, body)
	if len(gm.GroundingChunks) != 1 {
		t.Fatalf("同来源却写了 %d 个 chunk", len(gm.GroundingChunks))
	}
	if len(gm.GroundingSupports) != 2 {
		t.Fatalf("support 数 = %d, want 2", len(gm.GroundingSupports))
	}
}

// 没有区间就没有 support（Gemini 的 support 必须带 segment），
// 但来源本身要保住：chunk 照写。
func TestEncodeGroundingRangelessKeepsChunk(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "abc",
		Citations: []ir.Citation{{URL: "https://w"}}}}}
	body, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	gm := decodeGrounding(t, body)
	if gm == nil || len(gm.GroundingChunks) != 1 {
		t.Fatalf("来源丢了：%+v", gm)
	}
	if len(gm.GroundingSupports) != 0 {
		t.Fatalf("无区间却写了 support：%+v", gm.GroundingSupports)
	}
}

// 无引用时整个 groundingMetadata 不出现：空对象会让客户端以为做过检索。
func TestEncodeGroundingAbsentWithoutCitations(t *testing.T) {
	body, err := codec{}.EncodeResponse(&ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "abc"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gm := decodeGrounding(t, body); gm != nil {
		t.Fatalf("无引用却产出了 groundingMetadata：%+v", gm)
	}
}

// partIndex 要指向该块在 parts 数组里的位置：块与 part 不是一一对应
// （thinking 也占 part），错位会让客户端把高亮打到思考文本上。
func TestEncodeGroundingPartIndex(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "想一下"}},
		{Type: ir.BlockText, Text: "abc", Citations: []ir.Citation{{URL: "https://w", CitedText: "abc"}}},
	}}
	body, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	gm := decodeGrounding(t, body)
	if len(gm.GroundingSupports) != 1 {
		t.Fatalf("support 数 = %d", len(gm.GroundingSupports))
	}
	if pi := gm.GroundingSupports[0].Segment.PartIndex; pi != 1 {
		t.Fatalf("partIndex = %d, want 1（thinking 占了 part 0）", pi)
	}
}

// 流式编码：引用走独立 chunk，parts 留空（重发正文会让客户端看到重复文字）。
// 同一语义正文块的多个网络增量仍属于一个 part，区间相对完整块正文。
func TestStreamEncodeGrounding(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	feed := func(ev ir.Event) [][]byte {
		t.Helper()
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		return frames
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "北京今天晴，"})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "明天有雨。"})
	frames := feed(ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨"}}})
	if len(frames) != 1 {
		t.Fatalf("帧数 = %d, want 1", len(frames))
	}
	payload := frames[0]
	i := len("data: ")
	var r generateResponse
	if err := json.Unmarshal(payload[i:], &r); err != nil {
		t.Fatalf("帧不是 data: JSON：%s", payload)
	}
	gm := r.Candidates[0].GroundingMetadata
	if gm == nil || len(gm.GroundingSupports) != 1 {
		t.Fatalf("流里没带 grounding：%s", payload)
	}
	seg := gm.GroundingSupports[0].Segment
	if seg.PartIndex != 0 {
		t.Errorf("partIndex = %d, want 0（两个增量仍是同一个正文 part）", seg.PartIndex)
	}
	if seg.StartIndex != 18 || seg.EndIndex != 30 {
		t.Errorf("区间 = [%d,%d)，want [18,30)（相对完整块正文）", seg.StartIndex, seg.EndIndex)
	}
	if seg.Text != "明天有雨" {
		t.Errorf("segment 文本 = %q", seg.Text)
	}
	if len(r.Candidates[0].Content.Parts) != 0 {
		t.Errorf("引用 chunk 重发了正文：%s", payload)
	}
}

// 横跨两个网络增量的引用仍是一条 support：两个增量属于同一语义 part。
func TestStreamEncodeGroundingKeepsOnePartAcrossDeltas(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	feed := func(ev ir.Event) [][]byte {
		t.Helper()
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		return frames
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "北京今天晴，"})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "明天有雨。"})
	frames := feed(ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "晴，明天"}}})
	var r generateResponse
	if err := json.Unmarshal(frames[0][len("data: "):], &r); err != nil {
		t.Fatal(err)
	}
	gm := r.Candidates[0].GroundingMetadata
	if len(gm.GroundingChunks) != 1 {
		t.Fatalf("来源数 = %d, want 1（同一 URL 共用一条 chunk）", len(gm.GroundingChunks))
	}
	if len(gm.GroundingSupports) != 1 {
		t.Fatalf("support 数 = %d, want 1（网络分片不拆语义 part）", len(gm.GroundingSupports))
	}
	seg := gm.GroundingSupports[0].Segment
	if seg.PartIndex != 0 || seg.StartIndex != 12 || seg.EndIndex != 24 || seg.Text != "晴，明天" {
		t.Errorf("segment = %+v, want part0 [12,24) \"晴，明天\"", seg)
	}
}

// 思考 part 与签名 part 都占全局部件序号，但同一思考块的多个网络增量
// 只能占一个 part；多算会把后续正文引用推到不存在的位置。
func TestStreamEncodeGroundingPartIndexShifts(t *testing.T) {
	build := func() [][]byte {
		enc := codec{}.NewStreamEncoder()
		feed := func(ev ir.Event) {
			t.Helper()
			if _, err := enc.Encode(ev); err != nil {
				t.Fatal(err)
			}
		}
		feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"})
		feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}})
		feed(ir.Event{Type: ir.EvThinkingDelta, Index: 0, Text: "想"})
		feed(ir.Event{Type: ir.EvThinkingDelta, Index: 0, Text: "一下"})
		feed(ir.Event{Type: ir.EvSigDelta, Index: 0, Text: "sig", SignatureFrom: Name})
		feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
		feed(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}})
		feed(ir.Event{Type: ir.EvTextDelta, Index: 1, Text: "结论"})
		frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 1,
			Citations: []ir.Citation{{URL: "https://w", CitedText: "结论"}}})
		if err != nil {
			t.Fatal(err)
		}
		return frames
	}
	var r generateResponse
	if err := json.Unmarshal(build()[0][len("data: "):], &r); err != nil {
		t.Fatal(err)
	}
	seg := r.Candidates[0].GroundingMetadata.GroundingSupports[0].Segment
	if seg.PartIndex != 2 {
		t.Errorf("partIndex = %d, want 2（thought part 0 + 签名 part 1）", seg.PartIndex)
	}
}

// 被门控掉的外族签名不下发也就不占部件序号——partIndex 与实发 parts 对齐。
func TestStreamEncodeGroundingGatedSigDoesNotShift(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	feed := func(ev ir.Event) {
		t.Helper()
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"})
	feed(ir.Event{Type: ir.EvSigDelta, Index: 0, Text: "sig", SignatureFrom: "anthropic"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "结论"})
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "结论"}}})
	if err != nil {
		t.Fatal(err)
	}
	var r generateResponse
	if err := json.Unmarshal(frames[0][len("data: "):], &r); err != nil {
		t.Fatal(err)
	}
	if pi := r.Candidates[0].GroundingMetadata.GroundingSupports[0].Segment.PartIndex; pi != 0 {
		t.Errorf("partIndex = %d, want 0（外族签名没下发不占位）", pi)
	}
}

// 第二个正文块的引用必须指到自己的 part：硬编码 partIndex 0 会把块 1 的
// 高亮打到块 0 的正文上。
func TestStreamEncodeGroundingSecondTextBlock(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	feed := func(ev ir.Event) {
		t.Helper()
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "第一段。"})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "续一段。"})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 1, Text: "第二块。"})
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 1,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "第二块"}}})
	if err != nil {
		t.Fatal(err)
	}
	var r generateResponse
	if err := json.Unmarshal(frames[0][len("data: "):], &r); err != nil {
		t.Fatal(err)
	}
	seg := r.Candidates[0].GroundingMetadata.GroundingSupports[0].Segment
	if seg.PartIndex != 1 {
		t.Errorf("partIndex = %d, want 1（块 0 的两个增量只占 part 0）", seg.PartIndex)
	}
	if seg.StartIndex != 0 || seg.EndIndex != 9 {
		t.Errorf("区间 = [%d,%d)，want [0,9)", seg.StartIndex, seg.EndIndex)
	}
}

// 引用反推不出区间且无来源可写时不发帧：空 chunk 是噪声。
func TestStreamEncodeGroundingEmpty(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{CitedText: "无 URL"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("空引用却发了帧：%s", frames)
	}
}

// 有 URL 但区间反推不出（引文不在正文里）：来源 chunk 必须留住，
// 只是没有 support——把来源也丢了会让客户端连出处都看不到。
func TestStreamEncodeGroundingKeepsChunkWithoutRange(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	feed := func(ev ir.Event) {
		t.Helper()
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "正文"})
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://w", Title: "W", CitedText: "不在正文里"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("来源 chunk 被丢了：%v", frames)
	}
	var r generateResponse
	if err := json.Unmarshal(frames[0][len("data: "):], &r); err != nil {
		t.Fatal(err)
	}
	gm := r.Candidates[0].GroundingMetadata
	if len(gm.GroundingChunks) != 1 {
		t.Errorf("来源数 = %d, want 1", len(gm.GroundingChunks))
	}
	if len(gm.GroundingSupports) != 0 {
		t.Errorf("无区间却有 support：%+v", gm.GroundingSupports)
	}
}

// functionCall part 同样占全局部件序号：工具调用在前时，正文块的引用
// partIndex 要把它数进去，漏数会把高亮打到 functionCall 上。
func TestStreamEncodeGroundingAfterFunctionCall(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	feed := func(ev ir.Event) {
		t.Helper()
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t1", Name: "f"}}})
	feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"a":1}`})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0}) // functionCall part 0
	feed(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 1, Text: "结论"})
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 1,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "结论"}}})
	if err != nil {
		t.Fatal(err)
	}
	var r generateResponse
	if err := json.Unmarshal(frames[0][len("data: "):], &r); err != nil {
		t.Fatal(err)
	}
	if pi := r.Candidates[0].GroundingMetadata.GroundingSupports[0].Segment.PartIndex; pi != 1 {
		t.Errorf("partIndex = %d, want 1（functionCall 占了 part 0）", pi)
	}
}

// 字符下标 -> 字节下标的边界。
func TestByteOffset(t *testing.T) {
	const s = "a北b"
	for _, c := range []struct{ in, want int }{{-1, 0}, {0, 0}, {1, 1}, {2, 4}, {3, 5}, {99, 5}} {
		if got := byteOffset(s, c.in); got != c.want {
			t.Errorf("byteOffset(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
