package ir

import "testing"

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

// 无 URL 的引用没有任何价值（客户端无处可跳），一律丢弃。
func TestDedupeCitationsDropsEmptyURL(t *testing.T) {
	if out := DedupeCitations([]Citation{{CitedText: "x"}}); out != nil {
		t.Fatalf("空 URL 却保留了：%+v", out)
	}
	if out := DedupeCitations(nil); out != nil {
		t.Fatalf("空输入却返回了 %+v", out)
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
