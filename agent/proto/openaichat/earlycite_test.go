package openaichat

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R103-8 先于正文到达的标注不再丢弃：引用的偏移量相对块内累积正文计算，
// 先开 text 块再贴标注偏移依然对齐（new-api appendAnnotationDelta 同款）。
// 旧行为是静默丢弃，出处信息蒸发且无人报得出。
func TestStreamAnnotationBeforeTextOpensBlock(t *testing.T) {
	dec := New().NewStreamDecoder()
	evs, err := dec.Feed("", `{"id":"c1","choices":[{"index":0,"delta":{"annotations":[
		{"type":"url_citation","url_citation":{"url":"https://w","title":"W","start_index":0,"end_index":2}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var sawCite bool
	for _, ev := range evs {
		if ev.Type == ir.EvCitation && len(ev.Citations) == 1 && ev.Citations[0].URL == "https://w" {
			sawCite = true
		}
	}
	if !sawCite {
		t.Fatalf("先于正文的标注被丢弃：%+v", evs)
	}
	// 随后到达的正文要落进同一个块，偏移才能对上。
	evs2, err := dec.Feed("", `{"id":"c1","choices":[{"index":0,"delta":{"content":"正文"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var textIdx, citeIdx = -1, -1
	for _, ev := range evs2 {
		if ev.Type == ir.EvTextDelta {
			textIdx = ev.Index
		}
	}
	for _, ev := range evs {
		if ev.Type == ir.EvCitation {
			citeIdx = ev.Index
		}
	}
	if textIdx == -1 || textIdx != citeIdx {
		t.Errorf("正文没落进标注所在的块（cite=%d text=%d）", citeIdx, textIdx)
	}
}
