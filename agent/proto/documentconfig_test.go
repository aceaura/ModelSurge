package proto_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R96：文档块上的两项 Anthropic 专属配置（context 用途旁注、citations.enabled
// 引用开关）在外族里没有对应字段。附件本体能不能投出去是另一回事（gemini 走
// inlineData 投得出去，OpenAI 两系投不出去），但这两项配置跨族一律丢，
// 且此前完全静默——客户端明明开了文档引用，回来一条都没有。

// docConfigResp 一个带 context 的文档块 + 一个带引用开关的文档块 + 一段正文。
// 两项计数刻意不对称（1 与 1 会被对称夹具掩盖接反，故下面用 2/1 的变体测措辞）。
func docConfigResp(ctx, cites int) *ir.Response {
	var blocks []ir.Block
	for i := 0; i < ctx; i++ {
		blocks = append(blocks, ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
			Kind: ir.MediaDocument, MediaType: "application/pdf", Data: "UEQ=",
			Context: "how to read this contract",
		}})
	}
	for i := 0; i < cites; i++ {
		blocks = append(blocks, ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
			Kind: ir.MediaDocument, MediaType: "application/pdf", Data: "UEQ=",
			CitationsEnabled: boolPtr(true),
		}})
	}
	blocks = append(blocks, ir.Block{Type: ir.BlockText, Text: "Here is the file."})
	return &ir.Response{
		ID: "r", Model: "m", StopReason: ir.StopEndTurn,
		Content: blocks, Usage: ir.Usage{OutputTokens: 3},
	}
}

// 措辞对两个计数必须不对称，否则「两个计数接反」这条变异杀不掉。
func TestDocumentConfigDropNoteIsAsymmetric(t *testing.T) {
	if proto.DocumentConfigDropNote(2, 1) == proto.DocumentConfigDropNote(1, 2) {
		t.Fatal("注记措辞对两个计数对称，测不出计数接反")
	}
}

func TestDocumentConfigDropNoteSubjectShapes(t *testing.T) {
	for _, tc := range []struct {
		ctx, cites int
		wantPrefix string
		drop       []string
	}{
		{1, 0, "dropped the usage context on 1 document(s):", []string{"citation switch"}},
		{0, 1, "dropped the citation switch on 1 document(s):", []string{"usage context"}},
		{2, 3, "dropped the usage context on 2 document(s) and the citation switch on 3 document(s):", nil},
	} {
		got := proto.DocumentConfigDropNote(tc.ctx, tc.cites)
		if !strings.HasPrefix(got, tc.wantPrefix) {
			t.Errorf("(%d,%d) 前缀 = %q，want %q", tc.ctx, tc.cites, got, tc.wantPrefix)
		}
		for _, bad := range tc.drop {
			if strings.Contains(got, bad) {
				t.Errorf("(%d,%d) 渲染了计数为零的 %q：%s", tc.ctx, tc.cites, bad, got)
			}
		}
	}
}

// 非流式响应扫描要为外族报出配置损耗，且不得顺手删掉正文。
func TestResponseScanReportsDocumentConfig(t *testing.T) {
	want := proto.DocumentConfigDropNote(1, 1)
	for _, name := range foreignInbound {
		t.Run(name, func(t *testing.T) {
			notes := proto.MustInbound(name).ResponseNotes(docConfigResp(1, 1))
			if !slices.Contains(notes, want) {
				t.Fatalf("非流式扫描未报文档配置损耗：notes=%q", notes)
			}
			assertNoSessionContent(t, notes)
			body, err := proto.MustInbound(name).EncodeResponse(docConfigResp(1, 1))
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if !strings.Contains(string(body), "Here is the file.") {
				t.Errorf("报了损耗却顺手删了正文：%s", body)
			}
		})
	}
}

// A3 后：anthropic 响应侧 ContentBlock 联合（stable+beta）没有 document 成员，
// 整块文档连同 context/citations 配置与本体一并丢弃。报 MediaOutputDropNote，
// 不再报 DocumentConfigDropNote（整块已丢，再单报配置是重复计数），正文保留。
func TestResponseScanDropsDocumentBlockForAnthropic(t *testing.T) {
	notes := proto.MustInbound("anthropic").ResponseNotes(docConfigResp(1, 1))
	if want := proto.MediaOutputDropNote(0, 2); !slices.Contains(notes, want) {
		t.Fatalf("anthropic 未报文档块丢弃：notes=%q，want 含 %q", notes, want)
	}
	for _, n := range notes {
		if strings.Contains(n, "usage context") || strings.Contains(n, "citation switch") {
			t.Errorf("整块已丢，不应再重复报配置损耗：%q", n)
		}
	}
	body, err := proto.MustInbound("anthropic").EncodeResponse(docConfigResp(1, 1))
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	for _, gone := range []string{`"context":"how to read this contract"`, `"citations":{"enabled":true}`, "UEQ="} {
		if strings.Contains(string(body), gone) {
			t.Errorf("anthropic 响应侧仍带回了应丢弃的文档内容 %s：%s", gone, body)
		}
	}
	if !strings.Contains(string(body), "Here is the file.") {
		t.Errorf("报了损耗却顺手删了正文：%s", body)
	}
}

// 没带配置的文档不得触发这条注记，否则每个 PDF 响应都会挂一条假损耗。
func TestResponseScanSilentWithoutDocumentConfig(t *testing.T) {
	resp := docConfigResp(0, 0)
	for _, name := range append(append([]string{}, foreignInbound...), "anthropic") {
		notes := proto.MustInbound(name).ResponseNotes(resp)
		for _, n := range notes {
			if strings.Contains(n, "usage context") || strings.Contains(n, "citation switch") {
				t.Errorf("%s 对无配置文档误报：%q", name, n)
			}
		}
	}
}

// 注记里不得出现配置内容本身（属客户端提示词）。
func TestDocumentConfigNoteCarriesNoSessionContent(t *testing.T) {
	n := proto.DocumentConfigDropNote(1, 1)
	for _, leak := range []string{"how to read this contract", "UEQ="} {
		if strings.Contains(n, leak) {
			t.Errorf("注记泄漏了会话内容 %q：%s", leak, n)
		}
	}
}
