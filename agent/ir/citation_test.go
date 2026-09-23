package ir

import (
	"encoding/json"
	"testing"
)

// 上游只给范围时按范围切出原文，切片口径必须是字符：按字节切中文会切出乱码。
func TestResolveCitedTextFromRange(t *testing.T) {
	const text = "北京今天晴，明天有雨。"
	got := ResolveCitedText(text, Citation{Start: 0, End: 5})
	if got != "北京今天晴" {
		t.Fatalf("cited text = %q, want %q", got, "北京今天晴")
	}
}

// 已有 cited_text 时原样保留，不得按范围重切：上游给的原文才是权威。
func TestResolveCitedTextPrefersGiven(t *testing.T) {
	if got := ResolveCitedText("abcdef", Citation{CitedText: "xyz", Start: 0, End: 3}); got != "xyz" {
		t.Fatalf("cited text = %q, want %q", got, "xyz")
	}
}

// 越界范围返回空而不是截断：截出来的片段与上游真正引用的不同，据此高亮会指错位置。
func TestResolveCitedTextOutOfRange(t *testing.T) {
	for name, c := range map[string]Citation{
		"end 超长":   {Start: 0, End: 99},
		"start 为负": {Start: -1, End: 3},
		"空范围":      {Start: 2, End: 2},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ResolveCitedText("abcdef", c); got != "" {
				t.Fatalf("越界却切出了 %q", got)
			}
		})
	}
}

// 上游只给原文片段时在正文里定位，返回的是字符下标而非字节下标。
func TestResolveRangeFromCitedText(t *testing.T) {
	const text = "北京今天晴，明天有雨。"
	start, end, ok := ResolveRange(text, Citation{CitedText: "明天有雨"})
	if !ok {
		t.Fatal("唯一匹配却定位失败")
	}
	if start != 6 || end != 10 {
		t.Fatalf("range = [%d,%d)，want [6,10)（字符口径）", start, end)
	}
}

// 片段在正文里出现多次时必须放弃：取首次出现会把高亮落在错误的那一处。
func TestResolveRangeAmbiguous(t *testing.T) {
	if _, _, ok := ResolveRange("go and go again", Citation{CitedText: "go"}); ok {
		t.Fatal("多处匹配却给出了范围")
	}
}

func TestResolveRangeNoMatch(t *testing.T) {
	if _, _, ok := ResolveRange("abc", Citation{CitedText: "zzz"}); ok {
		t.Fatal("正文里没有该片段却给出了范围")
	}
	if _, _, ok := ResolveRange("abc", Citation{}); ok {
		t.Fatal("既无范围也无原文却给出了范围")
	}
}

// 已带范围时原样返回，不再按 cited_text 重新定位。
func TestResolveRangePrefersGiven(t *testing.T) {
	start, end, ok := ResolveRange("go and go again", Citation{CitedText: "go", Start: 7, End: 9})
	if !ok || start != 7 || end != 9 {
		t.Fatalf("range = [%d,%d) ok=%v，want [7,9) true", start, end, ok)
	}
}

// 同一来源同一范围重复下发只保留一条：流式路径上引用会随多个 chunk 重复到达。
func TestDedupeCitations(t *testing.T) {
	out := DedupeCitations([]Citation{
		{URL: "https://a", Start: 0, End: 3},
		{URL: "https://a", Start: 0, End: 3},
		{URL: "https://a", Start: 5, End: 8}, // 同来源不同范围：两条正文各自有出处，不能并
		{URL: "https://b", Start: 0, End: 3},
	})
	if len(out) != 3 {
		t.Fatalf("去重后 %d 条，want 3：%+v", len(out), out)
	}
	if out[0].URL != "https://a" || out[2].URL != "https://b" {
		t.Errorf("去重打乱了顺序：%+v", out)
	}
}

// 去重不承担「丢掉空 URL」的职责：Anthropic 的文档类引用（char_location 等）
// 本来就没有 URL，靠 document_index 与页/块/字符下标定位。此前那一条规则让
// 官方五种形态里的四种在解码后被静默清空，客户端看不到模型引了哪份文档。
func TestDedupeCitationsKeepsURLLess(t *testing.T) {
	out := DedupeCitations([]Citation{{CitedText: "x"}})
	if len(out) != 1 {
		t.Fatalf("空 URL 的引用被丢了：%+v", out)
	}
	if out := DedupeCitations(nil); out != nil {
		t.Fatalf("空输入却返回了 %+v", out)
	}
}

// 带 Raw 的按原文比：文档类引用的 URL 与范围可能全空，只靠投影字段区分会把
// 「同一段文字引自两个不同文档」误判成重复而丢掉一条真实出处。
func TestDedupeCitationsUsesRawAsKey(t *testing.T) {
	out := DedupeCitations([]Citation{
		{CitedText: "晴", Raw: json.RawMessage(`{"type":"char_location","document_index":0}`)},
		{CitedText: "晴", Raw: json.RawMessage(`{"type":"char_location","document_index":0}`)},
		{CitedText: "晴", Raw: json.RawMessage(`{"type":"char_location","document_index":1}`)},
	})
	if len(out) != 2 {
		t.Fatalf("去重后 %d 条，want 2：%+v", len(out), out)
	}
}

// Portable 判据：外族的标注槽位（Chat/Responses 的 url_citation、Gemini 的
// groundingChunk）一律以 URL 为来源身份，没有 URL 就无从表达。
func TestCitationPortable(t *testing.T) {
	if (Citation{CitedText: "x"}).Portable() {
		t.Error("无 URL 却判为可跨协议")
	}
	if !(Citation{URL: "https://a"}).Portable() {
		t.Error("有 URL 却判为不可跨协议")
	}
}

// 计数覆盖多消息多块：诊断要报总条数，漏层会让影响面被低估。
// 与 CountCitations 分开数，是因为四个族里三个「有标注槽位但装不下文档类引用」，
// 整族布尔量看不见这种逐条损耗。
func TestCountNonPortableCitations(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockText, Text: "a", Citations: []Citation{
				{URL: "https://a"}, {CitedText: "文档引用", WireType: "char_location"},
			}},
			{Type: BlockText, Text: "b", Citations: []Citation{{WireType: "page_location"}}},
		}},
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "c",
			Citations: []Citation{{URL: "https://d"}}}}},
	}}
	if n := CountNonPortableCitations(req); n != 2 {
		t.Fatalf("CountNonPortableCitations = %d, want 2", n)
	}
}

// 计数覆盖多消息多块：诊断要报总条数，漏层会让影响面被低估。
func TestCountCitations(t *testing.T) {
	req := &Request{Messages: []Message{
		{Role: RoleAssistant, Content: []Block{
			{Type: BlockText, Text: "a", Citations: []Citation{{URL: "https://a"}, {URL: "https://b"}}},
			{Type: BlockText, Text: "b", Citations: []Citation{{URL: "https://c"}}},
		}},
		{Role: RoleAssistant, Content: []Block{{Type: BlockText, Text: "c", Citations: []Citation{{URL: "https://d"}}}}},
	}}
	if n := CountCitations(req); n != 4 {
		t.Fatalf("CountCitations = %d, want 4", n)
	}
	if n := CountCitations(&Request{}); n != 0 {
		t.Fatalf("空请求计数 = %d, want 0", n)
	}
}

func TestCitationHasRange(t *testing.T) {
	if (Citation{}).HasRange() {
		t.Error("零值却报有范围")
	}
	if !(Citation{Start: 1, End: 2}).HasRange() {
		t.Error("有范围却报无")
	}
}

// 聚合路径：引用在正文之后到达，必须累到已开的那个块上。
func TestAggregatorCitation(t *testing.T) {
	a := NewAggregator()
	a.Feed(Event{Type: EvMessageStart, MessageID: "m1"})
	a.Feed(Event{Type: EvBlockStart, Index: 0, Block: &Block{Type: BlockText}})
	a.Feed(Event{Type: EvTextDelta, Index: 0, Text: "北京今天晴"})
	a.Feed(Event{Type: EvCitation, Index: 0, Citations: []Citation{{URL: "https://w", Start: 0, End: 2}}})
	// 同一条重复到达不得让客户端看到两个来源
	a.Feed(Event{Type: EvCitation, Index: 0, Citations: []Citation{{URL: "https://w", Start: 0, End: 2}}})
	a.Feed(Event{Type: EvBlockStop, Index: 0})
	a.Feed(Event{Type: EvMessageStop})
	resp, _ := a.Finish()
	if len(resp.Content) != 1 {
		t.Fatalf("块数 = %d, want 1：%+v", len(resp.Content), resp.Content)
	}
	if got := resp.Content[0].Citations; len(got) != 1 || got[0].URL != "https://w" {
		t.Fatalf("引用没落到块上：%+v", got)
	}
}

// 块没开时的引用只能丢：凭它开新块会让客户端多出一个空文本块。
func TestAggregatorCitationNoOpenBlock(t *testing.T) {
	a := NewAggregator()
	a.Feed(Event{Type: EvMessageStart})
	a.Feed(Event{Type: EvCitation, Index: 0, Citations: []Citation{{URL: "https://w"}}})
	a.Feed(Event{Type: EvMessageStop})
	resp, _ := a.Finish()
	if len(resp.Content) != 0 {
		t.Fatalf("凭引用开出了块：%+v", resp.Content)
	}
}

func cloneCitation(t *testing.T, c Citation) Citation {
	t.Helper()
	r := (&Request{Messages: []Message{{Role: RoleAssistant, Content: []Block{
		{Type: BlockText, Text: "晴", Citations: []Citation{c}},
	}}}}).Clone()
	return r.Messages[0].Content[0].Citations[0]
}

// Clone 走 JSON 往返，而空 RawMessage 没有 omitempty 时会被序列化成字面量
// null、再读回成 4 字节。那样 anthropic 的「带 Raw 就原样带回」分支会把 null
// 当成上游原文塞进 citations 数组，出站请求变成 "citations":[null]——上游整轮
// 被拒，引用本身也没了。跨协议投影来的引用（Chat 的 url_citation 等）本来就
// 没有 Raw，正是这条路径的常态。
func TestCloneDoesNotFabricateNullCitationRaw(t *testing.T) {
	got := cloneCitation(t, Citation{URL: "https://w", CitedText: "晴"})
	if len(got.Raw) != 0 {
		t.Fatalf("Clone 把空 Raw 变成了 %q", got.Raw)
	}
	if got.URL != "https://w" || got.CitedText != "晴" {
		t.Errorf("投影字段被 Clone 改坏了：%+v", got)
	}
}

// 反向也要钉住：真有 Raw 时 Clone 必须逐字节带回，否则 omitempty 修好了空值
// 却把不透明往返打穿了。
func TestCloneKeepsCitationRawByteExact(t *testing.T) {
	const raw = `{"type":"char_location","cited_text":"晴","document_index":0,"file_id":"file_abc"}`
	got := cloneCitation(t, Citation{CitedText: "晴", WireType: "char_location",
		Raw: json.RawMessage(raw)})
	if string(got.Raw) != raw {
		t.Fatalf("Raw 没逐字节带回：\n got %s\nwant %s", got.Raw, raw)
	}
}
