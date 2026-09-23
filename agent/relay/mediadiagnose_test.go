package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// mediadiagnose_test.go 非图片附件的有损诊断。
//
// 降级本身是必要的（目标协议没有槽位），但客户端必须知道它发的 PDF 变成了
// 一行占位文本，否则会以为附件已经送达、模型只是没读懂。

func mediaBlockReq(blocks ...ir.Block) *ir.Request {
	return &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: blocks}}}
}

func mediaOnly(kind ir.MediaKind, mime string) *ir.Request {
	return mediaBlockReq(ir.Block{Type: ir.BlockMedia, Media: &ir.Media{Kind: kind, MediaType: mime}})
}

// 各协议的媒体能力不同，诊断必须逐协议逐大类判，不得一概而论：
// anthropic 能收 document 收不了音频，OpenAI 两系两者都收，视频三家都收不了。
func TestDiagnoseMediaPerProtocol(t *testing.T) {
	for _, c := range []struct {
		proto string
		kind  ir.MediaKind
		mime  string
		warn  bool
	}{
		{"anthropic", ir.MediaDocument, "application/pdf", false},
		{"anthropic", ir.MediaAudio, "audio/wav", true},
		{"anthropic", ir.MediaVideo, "video/mp4", true},
		{"openai-chat", ir.MediaDocument, "application/pdf", false},
		{"openai-chat", ir.MediaAudio, "audio/wav", false},
		{"openai-chat", ir.MediaVideo, "video/mp4", true},
		{"openai-responses", ir.MediaDocument, "application/pdf", false},
		{"openai-responses", ir.MediaAudio, "audio/wav", false},
		{"openai-responses", ir.MediaVideo, "video/mp4", true},
	} {
		t.Run(c.proto+"/"+string(c.kind), func(t *testing.T) {
			notes := strings.Join(Diagnose(mediaOnly(c.kind, c.mime), c.proto, capsOf(t, c.proto)), "; ")
			got := strings.Contains(notes, "placeholder text block")
			if got != c.warn {
				t.Errorf("含媒体诊断 = %v, want %v；notes=%q", got, c.warn, notes)
			}
		})
	}
}

// 报的是哪个大类必须写清：读者的下一步动作不同（音频要先转文字，
// PDF 要先抽文本）。合成一条「附件装不下」等于没说。
func TestDiagnoseMediaNamesTheKind(t *testing.T) {
	for _, c := range []struct{ kind, mime, want string }{
		{string(ir.MediaAudio), "audio/wav", "audio attachment"},
		{string(ir.MediaVideo), "video/mp4", "video attachment"},
	} {
		t.Run(c.kind, func(t *testing.T) {
			notes := strings.Join(Diagnose(mediaOnly(ir.MediaKind(c.kind), c.mime), "anthropic", capsOf(t, "anthropic")), "; ")
			if !strings.Contains(notes, c.want) {
				t.Errorf("没说清大类，缺 %q：%q", c.want, notes)
			}
		})
	}
}

// 条数要报出来：客户端据此判断丢了一个还是十个。
func TestDiagnoseMediaCountsBlocks(t *testing.T) {
	aud := ir.Block{Type: ir.BlockMedia, Media: &ir.Media{Kind: ir.MediaAudio, MediaType: "audio/wav"}}
	notes := strings.Join(Diagnose(mediaBlockReq(aud, aud, aud), "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(notes, "3 audio attachment") {
		t.Errorf("未报出条数：%q", notes)
	}
}

// 多个大类同时装不下时各报一条：合成一条会让读者只处理其中一个。
func TestDiagnoseMediaReportsEachKindSeparately(t *testing.T) {
	req := mediaBlockReq(
		ir.Block{Type: ir.BlockMedia, Media: &ir.Media{Kind: ir.MediaDocument, MediaType: "application/pdf"}},
		ir.Block{Type: ir.BlockMedia, Media: &ir.Media{Kind: ir.MediaAudio, MediaType: "audio/wav"}},
		ir.Block{Type: ir.BlockMedia, Media: &ir.Media{Kind: ir.MediaVideo, MediaType: "video/mp4"}},
	)
	notes := Diagnose(req, "openai-chat", capsWithout(t, "openai-chat", "Documents", "Audio"))
	var n int
	for _, s := range notes {
		if strings.Contains(s, "placeholder text block") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("媒体诊断条数 = %d, want 3；notes=%v", n, notes)
	}
}

// tool 结果内嵌的附件同样会被降级。漏掉这层会让「工具返回了 PDF」
// 这类丢失完全不可见——顶层没有媒体块，诊断全绿。
func TestDiagnoseMediaInsideToolResult(t *testing.T) {
	req := mediaBlockReq(ir.Block{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
		ToolUseID: "t1",
		Content: []ir.Block{
			{Type: ir.BlockText, Text: "see attached"},
			{Type: ir.BlockMedia, Media: &ir.Media{Kind: ir.MediaAudio, MediaType: "audio/wav"}},
		},
	}})
	notes := strings.Join(Diagnose(req, "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(notes, "audio attachment") {
		t.Errorf("tool 结果里的附件丢失未被报出：%q", notes)
	}
}

// 载荷为 nil 的媒体块本身就是该报的丢失，静默跳过会漏报。
func TestDiagnoseNilMediaIsReported(t *testing.T) {
	req := mediaBlockReq(ir.Block{Type: ir.BlockMedia})
	notes := strings.Join(Diagnose(req, "openai-chat", capsWithout(t, "openai-chat", "Documents")), "; ")
	if !strings.Contains(notes, "unrecognized type") {
		t.Errorf("空媒体块未被报出：%q", notes)
	}
}

// MIME 认不出的附件按文档能力放行（文档槽位是各协议里最宽松的不透明容器），
// 只有连文档都装不下的上游才报。恒报会让这条诊断退化成噪声。
func TestDiagnoseUnknownMediaFollowsDocumentCapability(t *testing.T) {
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		t.Run(name, func(t *testing.T) {
			notes := strings.Join(Diagnose(mediaOnly(ir.MediaOther, "application/zip"), name, capsOf(t, name)), "; ")
			if strings.Contains(notes, "unrecognized type") {
				t.Errorf("有文档槽位却报了：%q", notes)
			}
		})
		t.Run(name+"/无文档槽位", func(t *testing.T) {
			notes := strings.Join(Diagnose(mediaOnly(ir.MediaOther, "application/zip"), name,
				capsWithout(t, name, "Documents")), "; ")
			if !strings.Contains(notes, "unrecognized type") {
				t.Errorf("连文档都装不下却没报：%q", notes)
			}
		})
	}
}

// 没有附件时不得报：恒报会让整条诊断链退化成噪声，读者会停止读它。
func TestDiagnoseNoMediaNoNote(t *testing.T) {
	req := mediaBlockReq(ir.Block{Type: ir.BlockText, Text: "hi"})
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		notes := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if strings.Contains(notes, "placeholder text block") {
			t.Errorf("%s 无附件却报了：%q", name, notes)
		}
	}
}

// 图片不走媒体诊断：它有独立的 Images / ImageURLs 两位与独立措辞，
// 卷进来会让同一次丢失被报两遍。
func TestDiagnoseImageNotCountedAsMedia(t *testing.T) {
	req := mediaBlockReq(ir.Block{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: "AAA="}})
	// 刻意把文档与音频位关掉：图片若被误当成非图片附件，这里必然报出占位文本。
	notes := strings.Join(Diagnose(req, "openai-chat", capsWithout(t, "openai-chat", "Documents", "Audio")), "; ")
	if strings.Contains(notes, "placeholder text block") {
		t.Errorf("图片被当成了非图片附件：%q", notes)
	}
}
