package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R93：Anthropic 文档类引用的跨族损耗注记，以及 search_result_location 的
// source 投影后能跨族。
//
// 官方 union 五种形态里只有 web_search_result_location 带 url、
// search_result_location 带 source；char_location / page_location /
// content_block_location 三种靠 document_index 与页号/块下标/字符下标定位，
// 根本没有 URL。外族的标注槽位（Chat/Responses 的 url_citation、Gemini 的
// groundingChunk）一律以 URL 为来源身份，这三种装不下——干净丢弃并报损耗，
// 同族往返则由 ir.Citation.Raw 原样带回。
//
// 修复前不是「丢弃」而是「静默蒸发」：整个 citations 数组只按 web_search 一种
// 形态解，四种没有 url 的在 DedupeCitations 的「空 URL 就丢」里被清空，
// 既没有错误也没有注记。

const docCiteText = "北京今天晴"

// docCitations 两条装不出本族的文档类引用（char_location + page_location）。
func docCitations() []ir.Citation {
	return []ir.Citation{
		{WireType: "char_location", CitedText: "晴", Title: "天气报告", Start: 4, End: 5,
			Raw: json.RawMessage(`{"type":"char_location","cited_text":"晴","document_index":0,` +
				`"document_title":"天气报告","start_char_index":4,"end_char_index":5,"file_id":"file_abc"}`)},
		{WireType: "page_location", CitedText: "晴", Title: "天气报告",
			Raw: json.RawMessage(`{"type":"page_location","cited_text":"晴","document_index":0,` +
				`"start_page_number":2,"end_page_number":3}`)},
	}
}

// searchResultCitation 一条能跨族的引用：source 投影到 URL 后外族装得下。
// 修复前它连解码都过不去（没有 url 键），source 上的来源 URL 一起丢。
func searchResultCitation() ir.Citation {
	return ir.Citation{WireType: "search_result_location", URL: "https://s", Title: "搜索结果", CitedText: "晴",
		Raw: json.RawMessage(`{"type":"search_result_location","cited_text":"晴","search_result_index":2,` +
			`"source":"https://s","title":"搜索结果","start_block_index":0,"end_block_index":1}`)}
}

// docCiteLeaks 文档类引用的原文特征串：任一条出现在外族线上都是伪造或泄漏。
var docCiteLeaks = []string{
	"char_location", "page_location", "document_index", "file_abc",
	"start_page_number", "天气报告",
}

func docCiteStream() []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: docCiteText},
		{Type: ir.EvCitation, Index: 0, Citations: append(docCitations(), searchResultCitation())},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
}

func docCiteResp() *ir.Response {
	return &ir.Response{ID: "m", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: docCiteText,
			Citations: append(docCitations(), searchResultCitation())}}}
}

func docCiteReq() *ir.Request {
	return &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "天气"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: docCiteText,
			Citations: append(docCitations(), searchResultCitation())}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go on"}}},
	}}
}

// 外族流式：文档类引用的定位字段与文档标题不得上线，正文与可跨族的那条保留。
func TestDocumentCitationStreamForeignSkip(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			out := streamOut(t, name, docCiteStream())
			for _, leak := range docCiteLeaks {
				if strings.Contains(out, leak) {
					t.Errorf("文档类引用泄漏 %q 进流：\n%s", leak, out)
				}
			}
			if !strings.Contains(out, docCiteText) {
				t.Errorf("正文被误删：\n%s", out)
			}
			if !strings.Contains(out, "https://s") {
				t.Errorf("可跨族的 search_result_location 被一起丢了：\n%s", out)
			}
		})
	}
}

// 同族流式：五条原样转出，一个字都不改写。
func TestDocumentCitationStreamAnthropicPassThrough(t *testing.T) {
	out := streamOut(t, "anthropic", docCiteStream())
	for _, c := range append(docCitations(), searchResultCitation()) {
		if !strings.Contains(out, string(c.Raw)) {
			t.Errorf("同族流改写了引用 %s：\n%s", c.Raw, out)
		}
	}
}

func TestDocumentCitationStreamForeignNotes(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			out, notes := streamOutputAndNotes(t, name, docCiteStream())
			got := strings.Join(notes, "; ")
			if !strings.Contains(got, "dropped 2 document citation(s)") {
				t.Errorf("应报 2 条丢失：%q", got)
			}
			for _, leak := range docCiteLeaks {
				if strings.Contains(got, leak) {
					t.Errorf("注记抄出会话内容 %q：%q", leak, got)
				}
			}
			if strings.Contains(out, "file_abc") {
				t.Errorf("file_id 泄漏进流：\n%s", out)
			}
		})
	}
	// anthropic 自家 encoder 不报：五种形态它都装得下。
	ea := proto.MustInbound("anthropic").NewStreamEncoder()
	for _, ev := range docCiteStream() {
		if _, err := ea.Encode(ev); err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
	}
	if notes := ea.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
}

// 非流式响应侧：ScanResponseLosses 报出；anthropic 装得下五种形态，静默；
// 全部可跨族时四族都静默——恒真的诊断等于没有诊断。
func TestDocumentCitationResponseNotes(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		got := strings.Join(proto.MustInbound(name).ResponseNotes(docCiteResp()), "; ")
		if !strings.Contains(got, "dropped 2 document citation(s)") {
			t.Errorf("%s 应报 2 条丢失：%q", name, got)
		}
	}
	if notes := proto.MustInbound("anthropic").ResponseNotes(docCiteResp()); len(notes) != 0 {
		t.Errorf("anthropic 误报（五种形态它都装得下）：%v", notes)
	}
	portable := docCiteResp()
	portable.Content[0].Citations = []ir.Citation{searchResultCitation()}
	empty := docCiteResp()
	empty.Content[0].Citations = nil
	for _, resp := range []*ir.Response{portable, empty} {
		for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
			if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
				t.Errorf("%s 无可丢引用时误报：%v", name, notes)
			}
		}
	}
}

// 非流式编码：外族线上不出现文档类引用的定位字段，正文与可跨族那条保留。
func TestDocumentCitationNotLeakedIntoResponse(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustInbound(name).EncodeResponse(docCiteResp())
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			for _, leak := range docCiteLeaks {
				if strings.Contains(string(body), leak) {
					t.Errorf("非流式响应泄漏 %q：%s", leak, body)
				}
			}
			if !strings.Contains(string(body), docCiteText) {
				t.Errorf("正文被误删：%s", body)
			}
			if !strings.Contains(string(body), "https://s") {
				t.Errorf("可跨族的那条被一起丢了：%s", body)
			}
		})
	}
}

// 请求方向：历史里的文档类引用投给外族上游时不得被塞进任何槽位。
// 塞一条空 url 的 url_citation 过去，客户端与上游都会渲染出一个跳不动的引用。
func TestDocumentCitationNotLeakedIntoForeignRequest(t *testing.T) {
	for _, name := range []string{"codex", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustOutbound(name).EncodeRequest(docCiteReq())
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			for _, leak := range docCiteLeaks {
				if strings.Contains(string(body), leak) {
					t.Errorf("出站请求泄漏 %q：%s", leak, body)
				}
			}
			if strings.Contains(string(body), `"url":""`) {
				t.Errorf("写出了空 url 的标注：%s", body)
			}
			if !strings.Contains(string(body), docCiteText) {
				t.Errorf("同一条消息的正文被误删：%s", body)
			}
		})
	}
}

// 同族请求方向：原样带回，encrypted_index 与 document_index 一个不少。
func TestDocumentCitationAnthropicRequestPassThrough(t *testing.T) {
	body, err := proto.MustOutbound("anthropic").EncodeRequest(docCiteReq())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	for _, c := range append(docCitations(), searchResultCitation()) {
		if !strings.Contains(string(body), string(c.Raw)) {
			t.Errorf("同族请求改写了引用 %s：%s", c.Raw, body)
		}
	}
}
