package relay

import (
	"slices"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R96：Anthropic 的 document 块除附件本体外还带两项配置——context（客户端给
// 模型的用途旁注）与 citations.enabled（文档引用开关）。外族的附件槽位只装
// 文件本身，两项配置跨族必丢，且此前完全静默：客户端明明开了文档引用，回来
// 一条都没有，也看不到任何迹象。附件本体照常投递，故与 mediaNotes 的大类
// 降级分开报。

// r96Doc 一个文档块，ctx 与 cites 各按零值表示「客户端没给这一项」。
func r96Doc(ctx string, cites *bool) ir.Block {
	return ir.Block{Type: ir.BlockMedia, Media: &ir.Media{
		Kind: ir.MediaDocument, MediaType: "application/pdf",
		Data: "UEQ=", Filename: "spec.pdf",
		Context: ctx, CitationsEnabled: cites,
	}}
}

// r96DocOn 引用开关显式打开。刻意不用共享变量取址：多处共用一个 *bool 会让
// 「三态压成两态」这类改动静默通过。
func r96DocOn() *bool { v := true; return &v }

// r96DocReq 2 个带 context 的文档 + 1 个开了引用开关的文档。计数刻意不对称：
// 对称夹具看不出「两个计数在调用点接反」，那条变异会存活。
func r96DocReq() *ir.Request {
	return &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			r96Doc("read the indemnity clause first", nil),
			r96Doc("compare against the 2024 revision", nil),
			r96Doc("", r96DocOn()),
		}}},
	}
}

func TestDiagnoseReportsDocumentConfig(t *testing.T) {
	want := proto.DocumentConfigDropNote(2, 1)
	for _, name := range foreignOutbound {
		t.Run(name, func(t *testing.T) {
			o := proto.MustOutbound(name)
			notes := Diagnose(r96DocReq(), name, o.Caps())
			if !slices.Contains(notes, want) {
				t.Fatalf("请求侧未报文档配置损耗：notes=%q", notes)
			}
			for _, n := range notes {
				for _, leak := range []string{"indemnity", "2024 revision", "spec.pdf", "UEQ="} {
					if strings.Contains(n, leak) {
						t.Errorf("注记带出了会话内容 %q：%s", leak, n)
					}
				}
			}
			body, err := o.EncodeRequest(r96DocReq())
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			// 报损耗的前提是配置真的丢了。
			for _, gone := range []string{"indemnity", "2024 revision", `"citations"`, `"context"`} {
				if strings.Contains(string(body), gone) {
					t.Errorf("谎报损耗：请求体里仍有 %q：%s", gone, body)
				}
			}
			// 但附件本体必须照常投递——这条注记说的是配置，不是文件。
			if !strings.Contains(string(body), "UEQ=") {
				t.Errorf("报了配置损耗却把附件本体也丢了：%s", body)
			}
		})
	}
}

// 原生形态不得报：anthropic 出站把两项配置原样写回，一条都没丢。
func TestDiagnoseSilentOnDocumentConfigForAnthropic(t *testing.T) {
	o := proto.MustOutbound("anthropic")
	notes := Diagnose(r96DocReq(), "anthropic", o.Caps())
	for _, n := range notes {
		if strings.Contains(n, "usage context") || strings.Contains(n, "citation switch") {
			t.Errorf("anthropic 谎报文档配置损耗：%s", n)
		}
	}
	body, err := o.EncodeRequest(r96DocReq())
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	for _, keep := range []string{
		`"context":"read the indemnity clause first"`,
		`"context":"compare against the 2024 revision"`,
		`"citations":{"enabled":true}`,
	} {
		if !strings.Contains(string(body), keep) {
			t.Errorf("anthropic 出站丢了 %s：%s", keep, body)
		}
	}
}

// tool 结果里内嵌的文档同样带配置。漏掉这层会让「工具返回了带引用开关的
// PDF」这类丢失完全不可见——顶层媒体块数得再准也报不出来。
func TestDiagnoseDocumentConfigInsideToolResult(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 64, Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
			ToolUseID: "t1",
			Content:   []ir.Block{{Type: ir.BlockText, Text: "see attached"}, r96Doc("tool fetched this", r96DocOn())},
		}},
	}}}}
	want := proto.DocumentConfigDropNote(1, 1)
	for _, name := range foreignOutbound {
		t.Run(name, func(t *testing.T) {
			notes := Diagnose(req, name, proto.MustOutbound(name).Caps())
			if !slices.Contains(notes, want) {
				t.Errorf("tool 结果里的文档配置丢失未被报出：notes=%q", notes)
			}
		})
	}
}

// 没带配置的普通文档不得触发这条注记：外族照样能收 PDF，恒报会让整条诊断链
// 退化成噪声，读者会停止读它。
func TestDiagnoseNoDocumentConfigNoNote(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 64, Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		r96Doc("", nil),
	}}}}
	for _, name := range append(append([]string{}, foreignOutbound...), "anthropic") {
		t.Run(name, func(t *testing.T) {
			notes := Diagnose(req, name, proto.MustOutbound(name).Caps())
			for _, n := range notes {
				if strings.Contains(n, "usage context") || strings.Contains(n, "citation switch") {
					t.Errorf("%s 对无配置文档误报：%q", name, n)
				}
			}
		})
	}
}

// 三态：显式关掉引用（false）与没表态（键缺失）不是一回事，两者都得报——
// 压成两态会让「客户端主动关掉」这一侧的往返不再逐字。
func TestDiagnoseReportsExplicitlyDisabledCitations(t *testing.T) {
	off := false
	req := &ir.Request{Model: "m", MaxTokens: 64, Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		r96Doc("", &off),
	}}}}
	o := proto.MustOutbound("anthropic")
	if notes := Diagnose(req, "anthropic", o.Caps()); len(notes) != 0 {
		for _, n := range notes {
			if strings.Contains(n, "citation switch") {
				t.Errorf("anthropic 对显式 false 谎报损耗：%s", n)
			}
		}
	}
	body, err := o.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(body), `"citations":{"enabled":false}`) {
		t.Errorf("显式 false 未原样带回：%s", body)
	}
	for _, name := range foreignOutbound {
		notes := Diagnose(req, name, proto.MustOutbound(name).Caps())
		if !slices.Contains(notes, proto.DocumentConfigDropNote(0, 1)) {
			t.Errorf("%s 未报显式 false 的开关损耗：notes=%q", name, notes)
		}
	}
}
