package gemini

import (
	"encoding/json"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R96c：附件块在 Gemini 流式里的 part 形态与部件序号。
// 本族用 inlineData / fileData 装本体（与非流式共用 mediaParts），所以附件
// **占一个 part 序号**：漏数会把随后正文的引用高亮打到附件上。

func feedMedia(t *testing.T, enc proto.StreamEncoder, events []ir.Event) [][]byte {
	t.Helper()
	var out [][]byte
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		out = append(out, frames...)
	}
	return out
}

func TestStreamMediaEmitsInlineDataParts(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	frames := feedMedia(t, enc, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockImage,
			Image: &ir.Image{MediaType: "image/png", Data: "SU1H"}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockMedia,
			Media: &ir.Media{Kind: ir.MediaDocument, MediaType: "application/pdf", Data: "UEQ=", Filename: "spec.pdf"}}},
		{Type: ir.EvBlockStop, Index: 1},
	})
	if len(frames) != 2 {
		t.Fatalf("两个附件应各下发一个 chunk，实得 %d：%q", len(frames), frames)
	}
	var first, second generateResponse
	if err := json.Unmarshal(frames[0][len("data: "):], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(frames[1][len("data: "):], &second); err != nil {
		t.Fatal(err)
	}
	p0 := first.Candidates[0].Content.Parts[0]
	if p0.InlineData == nil || p0.InlineData.MimeType != "image/png" || p0.InlineData.Data != "SU1H" {
		t.Errorf("图片 part 形态不对：%+v", p0)
	}
	p1 := second.Candidates[0].Content.Parts[0]
	if p1.InlineData == nil || p1.InlineData.MimeType != "application/pdf" || p1.InlineData.Data != "UEQ=" {
		t.Errorf("文档 part 形态不对：%+v", p1)
	}
	if notes := enc.Notes(); len(notes) != 0 {
		t.Errorf("本体投得出去却报了损耗：%v", notes)
	}
}

// 附件 part 占序号：随后的正文引用必须指到 part 1，而不是硬编码的 0。
func TestStreamMediaPartIndexShiftsGrounding(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	feedMedia(t, enc, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockImage,
			Image: &ir.Image{MediaType: "image/png", Data: "SU1H"}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 1, Text: "结论"},
	})
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 1,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "结论"}}})
	if err != nil {
		t.Fatal(err)
	}
	var r generateResponse
	if err := json.Unmarshal(frames[0][len("data: "):], &r); err != nil {
		t.Fatal(err)
	}
	seg := r.Candidates[0].GroundingMetadata.GroundingSupports[0].Segment
	if seg.PartIndex != 1 {
		t.Errorf("partIndex = %d, want 1（附件占了 part 0）", seg.PartIndex)
	}
}

// 装不下的附件（纯 file_id 引用、或只有 MIME 没有本体）不得下发任何 chunk：
// 伪造空 inlineData 会让客户端收到一个 mimeType 有值、data 为空的 part。
// 夹具刻意不对称（2 图片 + 1 文档），否则两个计数接反测不出来。
func TestStreamUndeliverableMediaEmitsNothing(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	frames := feedMedia(t, enc, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r1", Model: "gemini"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockMedia,
			Media: &ir.Media{Kind: ir.MediaDocument, MediaType: "application/pdf", FileID: "file_abc"}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png"}}},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/jpeg"}}},
		{Type: ir.EvBlockStop, Index: 2},
	})
	if len(frames) != 0 {
		t.Fatalf("装不下的附件不该下发 chunk：%q", frames)
	}
	notes := enc.Notes()
	if len(notes) != 1 {
		t.Fatalf("应合成一条注记，实得 %d：%q", len(notes), notes)
	}
	if want := proto.MediaOutputDropNote(2, 1); notes[0] != want {
		t.Errorf("注记 = %q, want %q", notes[0], want)
	}
	if again := enc.Notes(); len(again) != 0 {
		t.Errorf("Notes 应幂等排干：%v", again)
	}
}
