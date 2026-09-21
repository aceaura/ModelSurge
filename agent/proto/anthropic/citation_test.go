package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 非流式往返：text.citations 解出来要带范围与原文，编回去要还原成官方形态。
func TestCitationsRoundTrip(t *testing.T) {
	body := []byte(`{"id":"msg_1","model":"claude","role":"assistant","content":[
		{"type":"text","text":"北京今天晴，明天有雨。","citations":[
			{"type":"web_search_result_location","url":"https://w","title":"天气",
			 "cited_text":"明天有雨","encrypted_index":"idx1","start_char_index":6,"end_char_index":10}]}],
		"stop_reason":"end_turn"}`)
	resp, err := codec{}.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("块数 = %d：%+v", len(resp.Content), resp.Content)
	}
	cs := resp.Content[0].Citations
	if len(cs) != 1 {
		t.Fatalf("引用条数 = %d：%+v", len(cs), cs)
	}
	c := cs[0]
	if c.URL != "https://w" || c.Title != "天气" || c.CitedText != "明天有雨" {
		t.Errorf("引用字段丢失：%+v", c)
	}
	if c.Start != 6 || c.End != 10 {
		t.Errorf("范围 = [%d,%d)，want [6,10)", c.Start, c.End)
	}
	if c.EncryptedIndex != "idx1" {
		t.Errorf("encrypted_index 未保留：%q", c.EncryptedIndex)
	}

	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"type":"web_search_result_location"`, `"url":"https://w"`, `"cited_text":"明天有雨"`,
		`"start_char_index":6`, `"end_char_index":10`, `"encrypted_index":"idx1"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("编码缺 %s：\n%s", want, s)
		}
	}
}

// 首字起始的引用不能丢起点：start_char_index=0 是合法值，
// omitempty 会让客户端把它当缺省而落到错误位置。
func TestEncodeCitationsKeepsZeroStartInJSON(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴",
		Citations: []ir.Citation{{URL: "https://w", CitedText: "北京"}}}}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"start_char_index":0`) {
		t.Fatalf("零起点被 omitempty 吞掉了：\n%s", s)
	}
}

// 流式帧里同样不得丢零起点。
func TestStreamEncodeCitationsKeepsZeroStartInJSON(t *testing.T) {
	enc := New().NewStreamEncoder()
	if _, err := enc.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}}); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "北京今天晴"}); err != nil {
		t.Fatal(err)
	}
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "北京"}}})
	if err != nil {
		t.Fatal(err)
	}
	var s string
	for _, fr := range frames {
		s += string(fr)
	}
	if !strings.Contains(s, `"start_char_index":0`) {
		t.Fatalf("流帧丢了零起点：\n%s", s)
	}
}

// 只给 cited_text 不给范围时，编码要按正文反推索引：索引是客户端定位高亮的依据。
func TestEncodeCitationsBackfillsRange(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴，明天有雨。",
		Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨"}}}}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"start_char_index":6`) || !strings.Contains(s, `"end_char_index":10`) {
		t.Fatalf("没按正文反推索引：\n%s", s)
	}
}

// 只给范围不给 cited_text 时按范围切出原文：cited_text 是官方必填字段。
func TestEncodeCitationsBackfillsCitedText(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴",
		Citations: []ir.Citation{{URL: "https://w", Start: 0, End: 2}}}}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"cited_text":"北京"`) {
		t.Fatalf("没按范围切出 cited_text：\n%s", s)
	}
}

// 既反推不出原文也定位不出范围时整条丢弃：带空 cited_text 发出去上游会 400，
// 丢一条引用好过整轮被拒。
func TestEncodeCitationsDropsUnresolvable(t *testing.T) {
	for name, c := range map[string]ir.Citation{
		"无范围无原文":  {URL: "https://w"},
		"原文不在正文里": {URL: "https://w", CitedText: "不存在的片段"},
		"范围越界":    {URL: "https://w", Start: 0, End: 999},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴", Citations: []ir.Citation{c}}}}
			out, err := codec{}.EncodeResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			if s := string(out); strings.Contains(s, "citations") {
				t.Fatalf("反推不出却写出了引用：\n%s", s)
			}
		})
	}
}

// 无 URL 的引用解码时丢弃：客户端无处可跳。
func TestDecodeCitationsDropsEmptyURL(t *testing.T) {
	if cs := decodeCitations([]citation{{Type: "web_search_result_location", CitedText: "x"}}); cs != nil {
		t.Fatalf("空 URL 却解出了引用：%+v", cs)
	}
}

// 流式解码：citations_delta 要转成 EvCitation 并落在同一块序号上。
func TestStreamDecodeCitationsDelta(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	if _, err := dec.Feed("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":
		{"type":"web_search_result_location","url":"https://w","title":"T","cited_text":"晴","start_char_index":4,"end_char_index":5}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvCitation {
		t.Fatalf("事件 = %+v，want 一条 EvCitation", evs)
	}
	if evs[0].Index != 0 {
		t.Errorf("块序号 = %d, want 0", evs[0].Index)
	}
	if got := evs[0].Citations; len(got) != 1 || got[0].URL != "https://w" || got[0].Start != 4 {
		t.Fatalf("引用内容不对：%+v", got)
	}
}

// citation 为 null 的畸形帧不得 panic，也不得产出空引用。
func TestStreamDecodeCitationsDeltaNil(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	evs, err := dec.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("空 citation 却产出了事件：%+v", evs)
	}
}

// 流式编码：引用在正文之后到达，索引与 cited_text 要在累积的正文上反推。
func TestStreamEncodeCitations(t *testing.T) {
	enc := New().NewStreamEncoder()
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
	feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "北京今天晴，"})
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "明天有雨。"})
	feed(ir.Event{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨"}}})
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	s := string(out)
	for _, want := range []string{
		`"type":"citations_delta"`, `"url":"https://w"`, `"cited_text":"明天有雨"`,
		`"start_char_index":6`, `"end_char_index":10`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("流缺 %s：\n%s", want, s)
		}
	}
}

// 块没开时的引用只能丢：凭它开新块会让客户端多出一个空文本块，
// 而引用要贴的那段正文根本不在里面。
func TestStreamEncodeCitationsWithoutOpenBlock(t *testing.T) {
	enc := New().NewStreamEncoder()
	// 引用自带原文与范围，不依赖正文反推：这样唯一能拦住它的就是「块未开」这道判断
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "x", Start: 0, End: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("块未开却发出了帧：%s", frames)
	}
}

// 请求侧历史消息里的引用同样要能解出来：多轮对话回传时出处不能凭空消失。
func TestDecodeRequestCitations(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":16,"messages":[
		{"role":"assistant","content":[{"type":"text","text":"晴","citations":[
			{"type":"web_search_result_location","url":"https://w","cited_text":"晴","start_char_index":0,"end_char_index":1}]}]}]}`)
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
	var probe struct {
		Messages []struct {
			Content []struct {
				Citations []citation `json:"citations"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	// normalize 会在首位补一条 user 消息，assistant 是最后一条
	last := probe.Messages[len(probe.Messages)-1]
	if len(last.Content) != 1 || len(last.Content[0].Citations) != 1 {
		t.Fatalf("请求编码丢了引用：%s", out)
	}
	if c := last.Content[0].Citations[0]; c.StartCharIndex != 0 || c.EndCharIndex != 1 {
		t.Fatalf("范围 = [%d,%d)，want [0,1)：首字起始的引用不能丢起点", c.StartCharIndex, c.EndCharIndex)
	}
}
