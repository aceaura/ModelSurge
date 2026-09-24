package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R103-8 编码侧对照：EvCitation 先于正文块到达时块没开，引用无处可贴只能
// 丢弃——但必须计数报出（R95 判据）。旧实现直接 return，注记三条路径全空。
func TestStreamEncodeCitationBeforeBlockCounted(t *testing.T) {
	enc := New().NewStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 3,
		Citations: []ir.Citation{{URL: "https://w"}, {URL: "https://x"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		if strings.Contains(string(f), "https://") {
			t.Errorf("块没开时引用不该下发：%s", f)
		}
	}
	notes := enc.(interface{ Notes() []string }).Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "2 citation(s)") {
		t.Fatalf("先于块到达的引用没报损耗：%q", notes)
	}
	if strings.Contains(notes[0], "https://") {
		t.Errorf("注记带出了引用 URL（会话内容）：%s", notes[0])
	}
	if again := enc.(interface{ Notes() []string }).Notes(); len(again) != 0 {
		t.Errorf("Notes() 未排干：%q", again)
	}
}

// 对照组：块已开时引用照常下发且不计数（原生形态不许谎报）。
func TestStreamEncodeCitationOnOpenBlockNotCounted(t *testing.T) {
	enc := New().NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "正文"},
		{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{URL: "https://w", Start: 0, End: 2}}},
	} {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	if notes := enc.(interface{ Notes() []string }).Notes(); len(notes) != 0 {
		t.Errorf("块内引用不该有损耗注记：%q", notes)
	}
}
