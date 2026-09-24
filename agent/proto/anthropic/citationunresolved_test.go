package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R104：cited_text 反推失败的引用整条丢弃是对的（带空 cited_text 发出整轮必
// 400），但静默丢不行——丢弃条数必须进损耗注记（R95 判据）。
// R109-B1 起判据扩到 Required 字段：encrypted_index/url 同为必填
// （citation_web_search_result_location_param.py，stable 与 beta 一致），
// 外来投影拿不到加密下标，缺键发出同样整轮 400。

const unresolvedCiteNote = "lack the required encrypted_index or a resolvable cited_text"

// 流式：EvCitation 上的跨族投影标注（无 Raw、无有效区间）反推不出来，
// 计数进 Notes。
func TestStreamEncoderUnresolvedCitationCounted(t *testing.T) {
	enc := New().NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "正文"},
		{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{
			{URL: "https://a.example", Title: "A"}, // 无 Raw 无区间：反推失败
			{Raw: []byte(`{"type":"web_search_result_location","url":"https://b.example","cited_text":"正文"}`)}, // 原生 Raw 不计入
		}},
	} {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	notes := enc.Notes()
	found := ""
	for _, n := range notes {
		if strings.Contains(n, unresolvedCiteNote) {
			found = n
		}
	}
	if !strings.Contains(found, "dropped 1 citation(s)") {
		t.Fatalf("反推失败计数 = %q（全部注记 %v），want dropped 1", found, notes)
	}
}

// 非流式：ResponseNotes 按同一条判据扫出来；原生 Raw 与字段齐全的投影不计入。
func TestResponseNotesUnresolvedCitationCounted(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "正文内容", Citations: []ir.Citation{
		{URL: "https://a.example"}, // 反推失败且缺 encrypted_index
		{URL: "https://c.example", CitedText: "正文", EncryptedIndex: "enc_1"},                               // 字段齐全且可反推，不计入
		{Raw: []byte(`{"type":"web_search_result_location","url":"https://b.example","cited_text":"正文"}`)}, // 原生 Raw，不计入
	}}}}
	notes := (codec{}).ResponseNotes(resp)
	found := ""
	for _, n := range notes {
		if strings.Contains(n, unresolvedCiteNote) {
			found = n
		}
	}
	if !strings.Contains(found, "dropped 1 citation(s)") {
		t.Fatalf("反推失败计数 = %q（全部注记 %v），want dropped 1", found, notes)
	}
}

// 全部可解析时不许误报。
func TestResponseNotesNoUnresolvedCitationNoNote(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "正文", Citations: []ir.Citation{
		{URL: "https://c.example", CitedText: "正文", EncryptedIndex: "enc_1"},
	}}}}
	for _, n := range (codec{}).ResponseNotes(resp) {
		if strings.Contains(n, unresolvedCiteNote) {
			t.Fatalf("可解析的引用被误报：%q", n)
		}
	}
}
