package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// R99 请求侧诊断：图片的分辨率档位（detail）与文件引用（file_id）两维，此前在
// IR 里根本没有位置，跨族丢失完全不可见。两者都不是「少个字段」那么轻：
//
//   - detail 决定上游怎么切图、进而决定输入 token 计费（low 固定 85 token，
//     high 按原图分块，量级差一个数量级）。丢掉之后客户端指定的成本控制在
//     账单上看得出、在请求里看不出，读者无从归因。
//   - file_id 是 Responses 一族图片槽位的第二种载体。图片字节从未内联进请求体，
//     本层也不代取上游文件服务，所以投给没有这一维的目标就是彻底没了——与
//     「URL 装不下」那条「换成 base64 即可」不同，读者无从补救，必须分开报。
//
// 还有一类与能力位无关：三个载体全空的图片编不成任何协议的合法部件，编码器
// 一律整个跳过，故一律报告。

// r99ImageReq 造一条只含一张图片的请求。
func r99ImageReq(img *ir.Image) *ir.Request {
	return &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockImage, Image: img},
	}}}}
}

// r99Notes 取真实能力声明下的诊断结论，拼成一句便于断言。
func r99Notes(t *testing.T, req *ir.Request, outbound string) string {
	t.Helper()
	return strings.Join(Diagnose(req, outbound, capsOf(t, outbound)), "; ")
}

// ---- detail ----

// Anthropic 的 image source 只有 base64 / url / text 三种，没有档位这一维：
// 投过去只能丢，且必须报出来。
func TestDiagnoseImageDetailDropReportedOnAnthropic(t *testing.T) {
	req := r99ImageReq(&ir.Image{URL: "https://example.com/a.png", Detail: "low"})
	got := r99Notes(t, req, "anthropic")
	if !strings.Contains(got, "resolution tier") {
		t.Errorf("anthropic 没报出档位丢失：%q", got)
	}
	if !strings.Contains(got, "1 image(s)") {
		t.Errorf("未报出条数：%q", got)
	}
	// 图片本体照常送达，注记不得说成整张图被丢。
	if strings.Contains(got, "no image input") || strings.Contains(got, "no payload") {
		t.Errorf("图片本体能送达，却报了整张丢弃：%q", got)
	}
}

// OpenAI 两系原生有 detail 槽位，不得误报。
func TestDiagnoseImageDetailNotReportedOnOpenAIFamilies(t *testing.T) {
	req := r99ImageReq(&ir.Image{URL: "https://example.com/a.png", Detail: "low"})
	for _, outbound := range []string{"openai-chat", "openai-responses", "codex"} {
		if got := r99Notes(t, req, outbound); strings.Contains(got, "resolution tier") {
			t.Errorf("%s 有 detail 槽位却报了丢失：%q", outbound, got)
		}
	}
}

// 客户端没指定档位时不得报：恒报会让这条诊断退化成噪声。
func TestDiagnoseAbsentImageDetailNotReported(t *testing.T) {
	req := r99ImageReq(&ir.Image{URL: "https://example.com/a.png"})
	if got := r99Notes(t, req, "anthropic"); strings.Contains(got, "resolution tier") {
		t.Errorf("客户端没发 detail，不应报丢失：%q", got)
	}
}

// 只计带档位的那几张：三张里两张带，报 2 而不是 3。
func TestDiagnoseImageDetailCountsOnlyTaggedImages(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png", Detail: "low"}},
		{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/b.png"}},
		{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/c.png", Detail: "high"}},
	}}}}
	got := r99Notes(t, req, "anthropic")
	if !strings.Contains(got, "resolution tier on 2 image(s)") {
		t.Errorf("应只计带档位的 2 张，得到 %q", got)
	}
}

// ---- file_id ----

// Responses 一族（含同形的 codex）装得下只凭文件引用的图片：既不报「无载荷」，
// 也不报「引用丢失」。
func TestDiagnoseFileIDOnlyImageDeliverableOnResponsesFamily(t *testing.T) {
	req := r99ImageReq(&ir.Image{FileID: "file_abc123"})
	for _, outbound := range []string{"openai-responses", "codex"} {
		if got := r99Notes(t, req, outbound); got != "" {
			t.Errorf("%s 装得下 file_id 却报了损耗：%q", outbound, got)
		}
	}
}

// 投给没有这一维的目标必须报，且措辞要说清「无从补救」：图片字节从未内联进
// 请求体，本层也不代取上游文件服务。
func TestDiagnoseFileIDOnlyImageDropReportedOnSlotlessTargets(t *testing.T) {
	req := r99ImageReq(&ir.Image{FileID: "file_abc123"})
	for _, outbound := range []string{"anthropic", "openai-chat"} {
		got := r99Notes(t, req, outbound)
		if !strings.Contains(got, "file reference") {
			t.Errorf("%s 没报出文件引用丢失：%q", outbound, got)
		}
		if !strings.Contains(got, "never inlined") {
			t.Errorf("%s 的措辞没说清字节不在请求体里、读者无从补救：%q", outbound, got)
		}
	}
}

// 同一张图不得报两次：file_id 是唯一载体时归到更具体的那条，不再计入「无载荷」，
// 否则读者会以为丢了两张图。
func TestDiagnoseFileIDDropNotDoubleReportedAsPayloadless(t *testing.T) {
	req := r99ImageReq(&ir.Image{FileID: "file_abc123"})
	got := r99Notes(t, req, "anthropic")
	if strings.Contains(got, "no payload") {
		t.Errorf("同一张图被两条注记重复报告：%q", got)
	}
	if n := strings.Count(got, "image(s)"); n != 1 {
		t.Errorf("图片相关注记条数 = %d, want 1：%q", n, got)
	}
}

// 图片本体在场时，顺带那个目标不认的 file_id 不值得单报：本体已经送达，
// 引用只是冗余载体。报了会让读者以为这张图没送到。
func TestDiagnoseRedundantFileIDAlongsidePayloadNotReported(t *testing.T) {
	req := r99ImageReq(&ir.Image{URL: "https://example.com/a.png", FileID: "file_abc123"})
	if got := r99Notes(t, req, "anthropic"); got != "" {
		t.Errorf("图片本体能送达，不该报损耗：%q", got)
	}
}

// ---- 无载荷：与能力位无关 ----

// 三个载体全空的图片编不成任何协议的合法部件（anthropic 缺 media_type/data、
// chat 写出 url:""、responses 连 image_url 键都没有），编码器一律整个跳过，
// 所以四个出站都必须报。
func TestDiagnosePayloadlessImageReportedOnEveryOutbound(t *testing.T) {
	req := r99ImageReq(&ir.Image{})
	for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
		got := r99Notes(t, req, outbound)
		if !strings.Contains(got, "no payload") {
			t.Errorf("%s 没报出无载荷图片被跳过：%q", outbound, got)
		}
		if !strings.Contains(got, "would be rejected upstream") {
			t.Errorf("%s 的措辞没说清照编会被上游拒：%q", outbound, got)
		}
	}
}

// 只有 MIME、没有字节，同样是空壳。这是最容易漏的一态：解码器从 data URI 拆出
// 了 media_type，但 base64 段是空的。
func TestDiagnoseMediaTypeOnlyImageIsPayloadless(t *testing.T) {
	req := r99ImageReq(&ir.Image{MediaType: "image/png"})
	if got := r99Notes(t, req, "openai-chat"); !strings.Contains(got, "no payload") {
		t.Errorf("只有 MIME 的图片应算无载荷：%q", got)
	}
}

// nil 指针不得让诊断 panic，且与空壳同口径报告。
func TestDiagnoseNilImagePointerReportedNotPanicking(t *testing.T) {
	req := r99ImageReq(nil)
	for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
		if got := r99Notes(t, req, outbound); !strings.Contains(got, "no payload") {
			t.Errorf("%s 没报出 nil 图片：%q", outbound, got)
		}
	}
}

// 有效图片不得被牵连：同轮里一张正常、一张空壳，只报空壳那一张。
func TestDiagnosePayloadlessCountExcludesDeliverableImages(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
		{Type: ir.BlockImage, Image: &ir.Image{}},
		{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "QUJD"}},
	}}}}
	got := r99Notes(t, req, "openai-chat")
	if !strings.Contains(got, "no payload") || !strings.Contains(got, "1 image(s)") {
		t.Errorf("应只计空壳那 1 张，得到 %q", got)
	}
	if strings.Contains(got, "remote URLs") {
		t.Errorf("能送达的 URL 形态图片被误报：%q", got)
	}
}

// 整协议无图片能力时只出那一条总诊断：四条细粒度的图片注记都被它涵盖，
// 一起报等于把同一个丢失说五遍。
//
// 夹具必须同时踩满四条细粒度计数，缺一条就只证了一半。此前少了纯空壳那张，
// noPayload 恒为 0，于是把总诊断的触发条件偷偷加上「noPayload == 0」也照样
// 通过——而那个改动会让「一个空壳图片投给无图片能力的协议」退化成五条注记。
func TestDiagnoseNoImagesSupersedesNewImageNotes(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		// 远程 URL 形态：目标既不收图片、也不收 URL。
		{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png", Detail: "low"}},
		// 只给文件引用：目标没有文件引用槽位。
		{Type: ir.BlockImage, Image: &ir.Image{Detail: "low", FileID: "f1"}},
		// 纯空壳：与能力位无关的那一条。
		{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png"}},
	}}}}
	got := Diagnose(req, "hypothetical", proto.Capabilities{Images: false, ImageURLs: false, Sampling: true})
	if len(got) != 1 || !strings.Contains(got[0], "no image input") {
		t.Errorf("无图片能力应只出一条总诊断，得到 %v", got)
	}
	if !strings.Contains(got[0], "3 image(s)") {
		t.Errorf("总诊断该按图片总数计数，得到 %v", got)
	}
}

// 工具结果里内嵌的图片同样过这条路径：漏掉这层会让「工具返回了一张空壳图片」
// 这类丢失完全不可见。
func TestDiagnoseImageInsideToolResultIsCounted(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{
			{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png", Detail: "low"}},
			{Type: ir.BlockImage, Image: &ir.Image{}},
		}}},
	}}}}
	got := r99Notes(t, req, "anthropic")
	if !strings.Contains(got, "resolution tier") {
		t.Errorf("工具结果里图片的档位丢失没报出：%q", got)
	}
	if !strings.Contains(got, "no payload") {
		t.Errorf("工具结果里的空壳图片没报出：%q", got)
	}
}
