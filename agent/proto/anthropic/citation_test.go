package anthropic

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 官方 union 五种形态的最小合法样本。字段集直接取自本机 Anthropic SDK 的
// types/citation_*_location.py 与对应的 _param.py（请求与响应两侧一致），
// 不是从参考仓抄的——new-api 与 sub2api 都自己发明过 url 字段写进
// web_search_result_location，照抄会把伪造形状当成真相钉进测试。
var fiveOfficialCitations = []string{
	`{"type":"char_location","cited_text":"晴","document_index":0,"document_title":"天气报告",` +
		`"start_char_index":4,"end_char_index":5,"file_id":"file_abc"}`,
	`{"type":"page_location","cited_text":"晴","document_index":0,"document_title":"天气报告",` +
		`"start_page_number":2,"end_page_number":3}`,
	`{"type":"content_block_location","cited_text":"晴","document_index":1,` +
		`"start_block_index":0,"end_block_index":1}`,
	`{"type":"search_result_location","cited_text":"晴","search_result_index":2,"source":"https://s",` +
		`"title":"搜索结果","start_block_index":0,"end_block_index":1}`,
	`{"type":"web_search_result_location","url":"https://w","title":"T","cited_text":"晴",` +
		`"encrypted_index":"idx1"}`,
}

func citationsArray() string { return "[" + strings.Join(fiveOfficialCitations, ",") + "]" }

func respBody(citations string) []byte {
	return []byte(`{"id":"msg_1","model":"claude","role":"assistant","content":[
		{"type":"text","text":"北京今天晴，明天有雨。","citations":` + citations + `}],
		"stop_reason":"end_turn"}`)
}

// 官方五种形态解码后一条都不能少。此前整个数组只按 web_search_result_location
// 一种形态解，另外四种没有 url 键，在 DedupeCitations 的「空 URL 就丢」里被
// 静默清空——五种进去只剩一种出来，既没有错误也没有损耗注记，客户端看不到
// 模型引了哪份文档的哪一段。
func TestDecodeCitationsKeepsAllFiveOfficialTypes(t *testing.T) {
	resp, err := codec{}.DecodeResponse(respBody(citationsArray()))
	if err != nil {
		t.Fatal(err)
	}
	cs := resp.Content[0].Citations
	if len(cs) != len(fiveOfficialCitations) {
		t.Fatalf("引用条数 = %d，want %d：%+v", len(cs), len(fiveOfficialCitations), cs)
	}
	want := []struct {
		wireType  string
		url       string
		title     string
		portable  bool
		hasRange  bool
		encrypted string
	}{
		{"char_location", "", "天气报告", false, true, ""},
		{"page_location", "", "天气报告", false, false, ""},
		{"content_block_location", "", "", false, false, ""},
		// search_result_location 没有 url 键，来源 URL 在 source 上：投影过去
		// 才能跨族表达，否则它会被当成文档类引用一起丢。
		{"search_result_location", "https://s", "搜索结果", true, false, ""},
		{"web_search_result_location", "https://w", "T", true, false, "idx1"},
	}
	for i, w := range want {
		c := cs[i]
		if c.WireType != w.wireType {
			t.Errorf("[%d] WireType = %q，want %q", i, c.WireType, w.wireType)
		}
		if c.URL != w.url {
			t.Errorf("[%d] URL = %q，want %q", i, c.URL, w.url)
		}
		if c.Title != w.title {
			t.Errorf("[%d] Title = %q，want %q（文档标题在 document_title 键上）", i, c.Title, w.title)
		}
		if c.CitedText != "晴" {
			t.Errorf("[%d] CitedText = %q，want 晴", i, c.CitedText)
		}
		if c.Portable() != w.portable {
			t.Errorf("[%d] Portable = %v，want %v", i, c.Portable(), w.portable)
		}
		if c.HasRange() != w.hasRange {
			t.Errorf("[%d] HasRange = %v，want %v（[%d,%d)）", i, c.HasRange(), w.hasRange, c.Start, c.End)
		}
		if c.EncryptedIndex != w.encrypted {
			t.Errorf("[%d] EncryptedIndex = %q，want %q", i, c.EncryptedIndex, w.encrypted)
		}
		if len(c.Raw) == 0 {
			t.Errorf("[%d] Raw 为空：同族往返无从原样带回", i)
		}
	}
}

// 同族往返必须逐字还原：文档类引用的 document_index、页号、块下标、file_id
// 在 IR 之外无处可放，逐字段重建等于伪造。原样带回是唯一无损做法。
func TestCitationsRoundTripIsByteExact(t *testing.T) {
	resp, err := codec{}.DecodeResponse(respBody(citationsArray()))
	if err != nil {
		t.Fatal(err)
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Content []struct {
			Citations []json.RawMessage `json:"citations"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	got := probe.Content[0].Citations
	if len(got) != len(fiveOfficialCitations) {
		t.Fatalf("编回引用条数 = %d，want %d：%s", len(got), len(fiveOfficialCitations), out)
	}
	for i, want := range fiveOfficialCitations {
		if strings.TrimSpace(string(got[i])) != want {
			t.Errorf("[%d] 编回不是原文：\n got %s\nwant %s", i, got[i], want)
		}
	}
}

// 请求侧历史消息里的引用同样要五种全留、原样带回：多轮对话回传时出处不能
// 凭空消失，而 encrypted_index 丢了上游会拒绝续话。
func TestDecodeRequestCitationsKeepsAllFiveTypes(t *testing.T) {
	body := []byte(`{"model":"claude","max_tokens":16,"messages":[
		{"role":"assistant","content":[{"type":"text","text":"北京今天晴","citations":` +
		citationsArray() + `}]}]}`)
	req, err := codec{}.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if n := ir.CountCitations(req); n != len(fiveOfficialCitations) {
		t.Fatalf("请求侧引用数 = %d，want %d", n, len(fiveOfficialCitations))
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range fiveOfficialCitations {
		if !strings.Contains(string(out), want) {
			t.Errorf("请求编回丢了 %s：\n%s", want, out)
		}
	}
}

// 跨协议投影来的引用（没有 Raw）只能落进 web_search_result_location，且字段集
// 必须严格照官方 schema。此前凭空多出 start_char_index / end_char_index——
// 那两个键属 char_location，这一形态根本没有它们，按判别式校验的上游会当非法
// 输入拒掉整个请求；而客户端定位靠的是 cited_text，索引本来就用不上。
func TestEncodeSynthesizedCitationUsesOfficialFieldSet(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴",
		Citations: []ir.Citation{{URL: "https://w", Title: "T", CitedText: "晴", EncryptedIndex: "idx1"}}}}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Content []struct {
			Citations []map[string]any `json:"citations"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	cs := probe.Content[0].Citations
	if len(cs) != 1 {
		t.Fatalf("引用条数 = %d，want 1：%s", len(cs), out)
	}
	keys := make([]string, 0, len(cs[0]))
	for k := range cs[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"cited_text", "encrypted_index", "title", "type", "url"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("字段集 = %v，want %v：%s", keys, want, out)
	}
	if cs[0]["type"] != "web_search_result_location" {
		t.Errorf("type = %v，want web_search_result_location", cs[0]["type"])
	}
}

// 只给范围不给 cited_text 时按范围切出原文：cited_text 是官方必填字段。
func TestEncodeCitationsBackfillsCitedText(t *testing.T) {
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴",
		Citations: []ir.Citation{{URL: "https://w", Start: 0, End: 2, EncryptedIndex: "idx"}}}}}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"cited_text":"北京"`) {
		t.Fatalf("没按范围切出 cited_text：\n%s", s)
	}
}

// cited_text 反推不出来时整条丢弃：带空 cited_text 发出去上游会 400，
// 丢一条引用好过整轮被拒。cited_text 已给但正文里找不到时**不**丢——
// 官方这一形态不要求范围，上游拿 cited_text 自己定位。
func TestEncodeCitationsDropsOnlyWhenCitedTextUnresolvable(t *testing.T) {
	drops := map[string]ir.Citation{
		"无范围无原文": {URL: "https://w"},
		"范围越界":   {URL: "https://w", Start: 0, End: 999},
	}
	for name, c := range drops {
		t.Run("丢弃/"+name, func(t *testing.T) {
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
	t.Run("保留/原文不在正文里", func(t *testing.T) {
		c := ir.Citation{URL: "https://w", CitedText: "不存在的片段", EncryptedIndex: "idx"}
		resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "北京今天晴", Citations: []ir.Citation{c}}}}
		out, err := codec{}.EncodeResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(out); !strings.Contains(s, `"cited_text":"不存在的片段"`) {
			t.Fatalf("上游给了 cited_text 却被丢掉：\n%s", s)
		}
	})
}

// 流式解码：citations_delta 要转成 EvCitation 并落在同一块序号上。
func TestStreamDecodeCitationsDelta(t *testing.T) {
	dec := codec{}.NewStreamDecoder()
	if _, err := dec.Feed("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := dec.Feed("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":`+
			fiveOfficialCitations[4]+`}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != ir.EvCitation {
		t.Fatalf("事件 = %+v，want 一条 EvCitation", evs)
	}
	if evs[0].Index != 0 {
		t.Errorf("块序号 = %d, want 0", evs[0].Index)
	}
	got := evs[0].Citations
	if len(got) != 1 || got[0].URL != "https://w" || got[0].EncryptedIndex != "idx1" {
		t.Fatalf("引用内容不对：%+v", got)
	}
}

// 流式帧里的文档类引用此前直接蒸发（Feed err=<nil> events=0）：解码按单一
// 形态建模，没有 url 的那四种在去重里被清空。流式与非流式必须同一口径。
func TestStreamDecodeCitationsDeltaKeepsDocumentTypes(t *testing.T) {
	for _, raw := range fiveOfficialCitations[:4] {
		t.Run(strings.Split(raw, `"`)[3], func(t *testing.T) {
			dec := codec{}.NewStreamDecoder()
			evs, err := dec.Feed("content_block_delta",
				`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":`+raw+`}}`)
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != 1 || len(evs[0].Citations) != 1 {
				t.Fatalf("事件 = %+v，want 一条 EvCitation 带一条引用", evs)
			}
			if strings.TrimSpace(string(evs[0].Citations[0].Raw)) != raw {
				t.Errorf("Raw 不是原文：\n got %s\nwant %s", evs[0].Citations[0].Raw, raw)
			}
		})
	}
}

// citation 缺失或为 null 的畸形帧不得 panic，也不得产出空引用。
func TestStreamDecodeCitationsDeltaNil(t *testing.T) {
	for _, frame := range []string{
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":null}}`,
	} {
		dec := codec{}.NewStreamDecoder()
		evs, err := dec.Feed("content_block_delta", frame)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != 0 {
			t.Fatalf("空 citation 却产出了事件：%+v", evs)
		}
	}
}

// 流式编码：上游下发的引用原样转出去，不改写字段。
func TestStreamEncodeCitationsPassesRawThrough(t *testing.T) {
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
	feed(ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "北京今天晴"})
	for _, raw := range fiveOfficialCitations {
		feed(ir.Event{Type: ir.EvCitation, Index: 0,
			Citations: []ir.Citation{{WireType: "x", Raw: json.RawMessage(raw)}}})
	}
	feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	s := string(out)
	for _, want := range fiveOfficialCitations {
		if !strings.Contains(s, want) {
			t.Errorf("流帧丢了 %s：\n%s", want, s)
		}
	}
}

// 流式编码跨协议投影来的引用：cited_text 在累积正文上反推，字段集照官方 schema。
func TestStreamEncodeSynthesizedCitation(t *testing.T) {
	enc := New().NewStreamEncoder()
	var out []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "北京今天晴，"},
		{Type: ir.EvTextDelta, Index: 0, Text: "明天有雨。"},
		{Type: ir.EvCitation, Index: 0, Citations: []ir.Citation{{URL: "https://w", CitedText: "明天有雨", EncryptedIndex: "idx"}}},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, fr := range frames {
			out = append(out, fr...)
		}
	}
	s := string(out)
	for _, want := range []string{`"type":"citations_delta"`, `"url":"https://w"`, `"cited_text":"明天有雨"`} {
		if !strings.Contains(s, want) {
			t.Errorf("流缺 %s：\n%s", want, s)
		}
	}
	if strings.Contains(s, "char_index") {
		t.Errorf("凭空造出了字符下标：\n%s", s)
	}
}

// 块没开时的引用只能丢：凭它开新块会让客户端多出一个空文本块，
// 而引用要贴的那段正文根本不在里面。
func TestStreamEncodeCitationsWithoutOpenBlock(t *testing.T) {
	enc := New().NewStreamEncoder()
	// 引用自带原文，不依赖正文反推：这样唯一能拦住它的就是「块未开」这道判断
	frames, err := enc.Encode(ir.Event{Type: ir.EvCitation, Index: 0,
		Citations: []ir.Citation{{URL: "https://w", CitedText: "x", Raw: json.RawMessage(`{"type":"web_search_result_location","url":"https://w","cited_text":"x"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("块未开却发出了帧：%s", frames)
	}
}

// citations 键承载 {"enabled":bool} 配置对象时（document / search_result 块）
// 不得当成引用数组解析。
func TestDecodeCitationsIgnoresConfigObject(t *testing.T) {
	body := []byte(`{"id":"m","model":"c","role":"assistant","content":[
		{"type":"text","text":"晴","citations":{"enabled":true}}],"stop_reason":"end_turn"}`)
	resp, err := codec{}.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if cs := resp.Content[0].Citations; cs != nil {
		t.Fatalf("配置对象被当成引用：%+v", cs)
	}
}
