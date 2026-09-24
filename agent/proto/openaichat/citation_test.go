package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 非流式往返：message.annotations 解出来要挂到文本块上，编回去要还原官方子对象形态。
func TestAnnotationsRoundTrip(t *testing.T) {
	body := []byte(`{"id":"c1","model":"gpt","choices":[{"index":0,"finish_reason":"stop","message":
		{"role":"assistant","content":"北京今天晴，明天有雨。","annotations":[
			{"type":"url_citation","url_citation":{"url":"https://w","title":"天气","start_index":6,"end_index":10}}]}}]}`)
	resp, err := codec{}.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("块数 = %d：%+v", len(resp.Content), resp.Content)
	}
	cs := resp.Content[0].Citations
	if len(cs) != 1 || cs[0].URL != "https://w" || cs[0].Title != "天气" || cs[0].Start != 6 || cs[0].End != 10 {
		t.Fatalf("引用解码不对：%+v", cs)
	}

	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"annotations"`, `"type":"url_citation"`, `"url":"https://w"`,
		`"start_index":6`, `"end_index":10`, `"cited_text":"明天有雨"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("编码缺 %s：\n%s", want, s)
		}
	}
}

// 首字起始的引用不能丢起点：start_index=0 是合法值。
func TestEncodeAnnotationsKeepsZeroStart(t *testing.T) {
	as := encodeAnnotations("北京今天晴", []ir.Citation{{URL: "https://w", Start: 0, End: 2}})
	if len(as) != 1 {
		t.Fatalf("条数 = %d", len(as))
	}
	if as[0].URLCitation.StartIndex != 0 || as[0].URLCitation.EndIndex != 2 {
		t.Fatalf("范围 = [%d,%d)", as[0].URLCitation.StartIndex, as[0].URLCitation.EndIndex)
	}
}

// 序列化层面也不得丢零起点：omitempty 会让客户端把它当缺省。
func TestEncodeResponseKeepsZeroStartInJSON(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴",
		Citations: []ir.Citation{{URL: "https://w", CitedText: "北京"}}}}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"start_index":0`) {
		t.Fatalf("零起点被 omitempty 吞掉了：\n%s", s)
	}
}

// 与 Anthropic 不同：反推不出范围时仍写出该条，URL 与标题本身有价值，
// 且 Chat 不把 cited_text 当必填，带零范围不会被拒。
func TestEncodeAnnotationsKeepsRangelessCitation(t *testing.T) {
	as := encodeAnnotations("北京今天晴", []ir.Citation{{URL: "https://w", Title: "T"}})
	if len(as) != 1 || as[0].URLCitation.URL != "https://w" {
		t.Fatalf("反推不出范围就整条丢了：%+v", as)
	}
	if as[0].URLCitation.StartIndex != 0 || as[0].URLCitation.EndIndex != 0 {
		t.Errorf("定位不出来却写了范围：%+v", as[0].URLCitation)
	}
}

// 平铺形态（少数兼容上游）也要收：官方是子对象，但平铺的同样只有这一种语义。
func TestDecodeAnnotationsRejectsUnknownType(t *testing.T) {
	if cs := decodeAnnotations([]annotation{{Type: "file_citation"}}); cs != nil {
		t.Fatalf("非 url_citation 却解出了引用：%+v", cs)
	}
	if cs := decodeAnnotations([]annotation{{Type: "url_citation"}}); cs != nil {
		t.Fatalf("子对象为 nil 却解出了引用：%+v", cs)
	}
	if cs := decodeAnnotations([]annotation{{Type: "url_citation", URLCitation: &urlCitation{}}}); cs != nil {
		t.Fatalf("空 URL 却解出了引用：%+v", cs)
	}
}

// 消息级标注挂到最后一个文本块上；没有文本块时整批丢弃（挂到图片块上偏移量无意义）。
func TestAttachCitations(t *testing.T) {
	cs := []ir.Citation{{URL: "https://w"}}
	blocks := attachCitations([]ir.Block{
		{Type: ir.BlockText, Text: "a"},
		{Type: ir.BlockText, Text: "b"},
	}, cs)
	if len(blocks[0].Citations) != 0 || len(blocks[1].Citations) != 1 {
		t.Fatalf("没挂到最后一个文本块：%+v", blocks)
	}
	only := attachCitations([]ir.Block{{Type: ir.BlockImage}}, cs)
	if len(only[0].Citations) != 0 {
		t.Fatalf("挂到了非文本块上：%+v", only)
	}
}

// 多个文本块会拼成一条 content，块内偏移量要整体平移，
// 否则第二个块的引用会指到第一个块的正文里。
func TestEncodeResponseShiftsOffsetsAcrossBlocks(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockText, Text: "北京今天晴。"},
		{Type: ir.BlockText, Text: "明天有雨。", Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨"}}},
	}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	// 前缀 6 字符 + 块内 [0,4) => [6,10)
	if s := string(out); !strings.Contains(s, `"start_index":6`) || !strings.Contains(s, `"end_index":10`) {
		t.Fatalf("跨块偏移量没平移：\n%s", s)
	}
}

// 请求侧同样要平移：历史 assistant 消息的多个文本块会被拼成一条 content。
// 两块正文刻意相同——只有先在本块内定位再加前缀长度才能定出唯一位置，
// 直接在拼接后的正文里找会因为出现两次而放弃范围。
func TestEncodeRequestShiftsOffsetsAcrossBlocks(t *testing.T) {
	req := &ir.Request{Model: "gpt", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "天气"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockText, Text: "明天有雨。"},
			{Type: ir.BlockText, Text: "明天有雨。", Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨"}}},
		}},
	}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	// 前缀 5 字符 + 块内 [0,4) => [5,9)
	if s := string(out); !strings.Contains(s, `"start_index":5`) || !strings.Contains(s, `"end_index":9`) {
		t.Fatalf("请求侧跨块偏移量没平移：\n%s", s)
	}
}

// 定位不出来时来源本身不得丢：范围留零，URL 与标题照旧送达。
func TestEncodeResponseKeepsSourceWhenUnlocatable(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockText, Text: "北京今天晴。"},
		{Type: ir.BlockText, Text: "明天有雨。", Citations: []ir.Citation{{URL: "https://w", CitedText: "不存在的片段"}}},
	}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"url":"https://w"`) {
		t.Fatalf("来源被整条丢了：\n%s", s)
	}
	if !strings.Contains(s, `"start_index":0`) || !strings.Contains(s, `"end_index":0`) {
		t.Fatalf("定位不出来却写了范围：\n%s", s)
	}
}

// 流式解码：delta.annotations 要转成 EvCitation 落到已开的 text 块上。
func TestStreamDecodeAnnotations(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	if _, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"content":"北京今天晴"}}]}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"annotations":[
		{"type":"url_citation","url_citation":{"url":"https://w","start_index":0,"end_index":2}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var got []ir.Citation
	for _, ev := range evs {
		if ev.Type == ir.EvCitation {
			got = append(got, ev.Citations...)
		}
	}
	if len(got) != 1 || got[0].URL != "https://w" {
		t.Fatalf("标注没解出来：%+v", evs)
	}
}

// 先于正文到达的标注不再丢弃（R103-8）：先开 text 块再贴引用，偏移相对块内
// 累积正文计算依然对齐（new-api appendAnnotationDelta 同款）。随后到达的正文
// 与引用同块。旧契约（块未开就丢标注）让出处信息静默蒸发且无人报得出。
func TestStreamDecodeAnnotationsWithoutTextBlock(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("data", `{"id":"c1","choices":[{"index":0,"delta":{"annotations":[
		{"type":"url_citation","url_citation":{"url":"https://w"}}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var got []ir.Citation
	for _, ev := range evs {
		if ev.Type == ir.EvCitation {
			got = append(got, ev.Citations...)
		}
	}
	if len(got) != 1 || got[0].URL != "https://w" {
		t.Fatalf("先于正文的标注被丢弃：%+v", evs)
	}
}

// 流式编码：引用单独一个 chunk 下发，索引在累积正文上反推。
func TestStreamEncodeAnnotations(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	var out []byte
	feed := func(ev ir.Event) {
		t.Helper()
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, fr := range frames {
			out = append(out, fr...)
		}
	}
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "c1", Model: "gpt"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "北京今天晴，"})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "明天有雨。"})
	feed(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨"}}})
	s := string(out)
	for _, want := range []string{`"annotations"`, `"url":"https://w"`, `"start_index":6`, `"end_index":10`} {
		if !strings.Contains(s, want) {
			t.Errorf("流缺 %s：\n%s", want, s)
		}
	}
}

// 空引用列表不得发出空 chunk：客户端会把它当成一次无内容的增量。
func TestStreamEncodeEmptyCitations(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("空引用却发了帧：%s", frames)
	}
}

// 请求侧：历史 assistant 消息里的标注要能解出来并原样编回。
func TestRequestAnnotationsRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt","messages":[
		{"role":"assistant","content":"晴","annotations":[
			{"type":"url_citation","url_citation":{"url":"https://w","start_index":0,"end_index":1}}]}]}`)
	req, err := codec{}.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if n := ir.CountCitations(req); n != 1 {
		t.Fatalf("请求侧引用数 = %d, want 1", n)
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"url":"https://w"`) {
		t.Fatalf("请求编码丢了引用：%s", out)
	}
}
