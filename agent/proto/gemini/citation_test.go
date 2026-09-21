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
	if seg := gm.GroundingSupports[0].Segment; seg.StartIndex != 18 || seg.EndIndex != 30 {
		t.Fatalf("流内区间 = [%d,%d)，want [18,30)：偏移量按累积正文算", seg.StartIndex, seg.EndIndex)
	}
	if len(r.Candidates[0].Content.Parts) != 0 {
		t.Errorf("引用 chunk 重发了正文：%s", payload)
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

// 字符下标 -> 字节下标的边界。
func TestByteOffset(t *testing.T) {
	const s = "a北b"
	for _, c := range []struct{ in, want int }{{-1, 0}, {0, 0}, {1, 1}, {2, 4}, {3, 5}, {99, 5}} {
		if got := byteOffset(s, c.in); got != c.want {
			t.Errorf("byteOffset(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
