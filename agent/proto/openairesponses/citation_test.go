package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 非流式往返：output_text.annotations 是平铺形态（不同于 Chat 的子对象）。
func TestAnnotationsRoundTrip(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"gpt","status":"completed","output":[
		{"type":"message","id":"msg_1","role":"assistant","content":[
			{"type":"output_text","text":"北京今天晴，明天有雨。","annotations":[
				{"type":"url_citation","url":"https://w","title":"天气","start_index":6,"end_index":10}]}]}]}`)
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
	for _, want := range []string{`"annotations"`, `"type":"url_citation"`, `"url":"https://w"`, `"start_index":6`, `"end_index":10`} {
		if !strings.Contains(s, want) {
			t.Errorf("编码缺 %s：\n%s", want, s)
		}
	}
}

// 首字起始的引用不能丢起点，序列化层面也不得被 omitempty 吞掉。
func TestEncodeAnnotationsKeepsZeroStart(t *testing.T) {
	as := encodeAnnotations("北京今天晴", []ir.Citation{{URL: "https://w", Start: 0, End: 2}})
	if len(as) != 1 || as[0].StartIndex != 0 || as[0].EndIndex != 2 {
		t.Fatalf("范围不对：%+v", as)
	}
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴",
		Citations: []ir.Citation{{URL: "https://w", Start: 0, End: 2}}}}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"start_index":0`) {
		t.Fatalf("零起点被 omitempty 吞掉了：\n%s", s)
	}
}

// 该协议形态里没有 cited_text 字段，只能靠索引；定位不出来仍写出该条。
func TestEncodeAnnotationsKeepsRangelessCitation(t *testing.T) {
	as := encodeAnnotations("abc", []ir.Citation{{URL: "https://w", Title: "T"}})
	if len(as) != 1 || as[0].URL != "https://w" || as[0].EndIndex != 0 {
		t.Fatalf("反推不出范围时形态不对：%+v", as)
	}
}

func TestDecodeAnnotationsFilters(t *testing.T) {
	if cs := decodeAnnotations([]annotation{{Type: "file_citation", URL: "https://w"}}); cs != nil {
		t.Fatalf("非 url_citation 却解出了引用：%+v", cs)
	}
	if cs := decodeAnnotations([]annotation{{Type: "url_citation"}}); cs != nil {
		t.Fatalf("空 URL 却解出了引用：%+v", cs)
	}
}

// 回归：response.output_text.annotation.added 此前与文本增量并档，
// 而该事件没有 delta 字段，于是恒命中空分支被静默丢弃，引用一条都到不了客户端。
func TestStreamDecodeAnnotationAdded(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("", `{"type":"response.output_text.annotation.added","output_index":0,
		"annotation":{"type":"url_citation","url":"https://w","title":"T","start_index":0,"end_index":2}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvCitation {
		t.Fatalf("事件 = %+v，want 一条 EvCitation（并档回归）", evs)
	}
	if got := evs[0].Citations; len(got) != 1 || got[0].URL != "https://w" || got[0].End != 2 {
		t.Fatalf("引用内容不对：%+v", got)
	}
}

// annotation 为 null 的畸形帧不得 panic，也不得产出空引用。
func TestStreamDecodeAnnotationAddedNil(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("", `{"type":"response.output_text.annotation.added","output_index":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("空 annotation 却产出了事件：%+v", evs)
	}
}

// 文本增量仍走原路，不得被 annotation 分支吃掉。
func TestStreamDecodeTextDeltaUnaffected(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("", `{"type":"response.output_text.delta","output_index":0,"delta":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvTextDelta || evs[0].Text != "hi" {
		t.Fatalf("文本增量被破坏：%+v", evs)
	}
}

// 流式编码：既要发 annotation.added 增量，也要在 output_item.done 的 part 里
// 带全量 annotations——只发增量的话读 final response 的 SDK 拿不到任何引用。
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
	feed(ir.Event{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "gpt"})
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "北京今天晴，"})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "明天有雨。"})
	feed(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨"}}})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	s := string(out)
	for _, want := range []string{
		`"response.output_text.annotation.added"`, `"url":"https://w"`,
		`"start_index":6`, `"end_index":10`, `"response.output_item.done"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("流缺 %s：\n%s", want, s)
		}
	}
	// done 的 part 里必须也带上引用
	done := s[strings.LastIndex(s, `"response.output_item.done"`):]
	if !strings.Contains(done, `"url":"https://w"`) {
		t.Errorf("output_item.done 的 part 没带 annotations：\n%s", done)
	}
}

// 块没开时的引用只能丢。
func TestStreamEncodeCitationWithoutBlock(t *testing.T) {
	enc := codec{}.NewStreamEncoder()
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 3,
		Citations: []ir.Citation{{URL: "https://w"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("块未开却发出了帧：%s", frames)
	}
}

// 请求侧：历史 assistant 消息里的标注要能解出来并原样编回。
func TestRequestAnnotationsRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt","input":[
		{"type":"message","role":"assistant","content":[
			{"type":"output_text","text":"晴","annotations":[
				{"type":"url_citation","url":"https://w","start_index":0,"end_index":1}]}]}]}`)
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
