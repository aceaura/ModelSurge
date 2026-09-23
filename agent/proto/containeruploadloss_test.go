package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R69：container_upload 块的跨族损耗注记与跳过安全性。该块是 anthropic
// 专属（容器文件引用），外族没有槽位：整块跳过不降级（file_id 拼进正文
// 会污染回答），损耗经 ResponseNotes / 流式 Notes() 报出。

var r69Upload = ir.Block{Type: ir.BlockContainerUpload,
	ContainerUpload: &ir.ContainerUploadRef{FileID: "file_secret1"}}

func r69UploadStream() []ir.Event {
	return []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "chart ready"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &r69Upload},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
}

// 外族流式：块不出现在线上（file_id 不泄漏），正文不受影响，Notes 报丢失。
func TestContainerUploadStreamForeignSkip(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			out := streamOut(t, name, r69UploadStream())
			if strings.Contains(out, "file_secret1") || strings.Contains(out, "container_upload") {
				t.Errorf("file_id 泄漏进流：\n%s", out)
			}
			if !strings.Contains(out, "chart ready") {
				t.Errorf("正文被误删：\n%s", out)
			}
		})
	}
}

func TestContainerUploadStreamForeignNotes(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			e := proto.MustInbound(name).NewStreamEncoder()
			for _, ev := range r69UploadStream() {
				if _, err := e.Encode(ev); err != nil {
					t.Fatalf("Encode(%s): %v", ev.Type, err)
				}
			}
			got := strings.Join(e.Notes(), "; ")
			if !strings.Contains(got, "dropped 1 container upload block(s)") {
				t.Errorf("应报 1 块丢失：%q", got)
			}
			if again := e.Notes(); len(again) != 0 {
				t.Errorf("Notes 应幂等排干：%v", again)
			}
		})
	}
	// 多块计数聚合为一条。
	e := proto.MustInbound("openai-chat").NewStreamEncoder()
	e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
	e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &r69Upload})
	e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 0})
	e.Encode(ir.Event{Type: ir.EvBlockStart, Index: 1, Block: &r69Upload})
	e.Encode(ir.Event{Type: ir.EvBlockStop, Index: 1})
	e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	got := strings.Join(e.Notes(), "; ")
	if !strings.Contains(got, "dropped 2 container upload block(s)") {
		t.Errorf("多块应计数：%q", got)
	}
	// anthropic 自家 encoder 不报。
	ea := proto.MustInbound("anthropic").NewStreamEncoder()
	for _, ev := range r69UploadStream() {
		ea.Encode(ev)
	}
	if notes := ea.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
}

// responses 的 response.completed 带全量 output：被跳过的块不得从这里漏出，
// 也不得留下空壳 message item。
func TestContainerUploadAbsentFromResponsesFullOutput(t *testing.T) {
	out := streamOut(t, "openai-responses", r69UploadStream())
	idx := strings.LastIndex(out, `"type":"response.completed"`)
	if idx < 0 {
		t.Fatalf("没有 response.completed：\n%s", out)
	}
	if final := out[idx:]; strings.Contains(final, "file_secret1") {
		t.Errorf("全量 output 泄漏 file_id：\n%s", final)
	}
	if n := strings.Count(out, `"type":"response.output_item.added"`); n != 1 {
		t.Errorf("output_item.added 应只有正文一条，实得 %d：\n%s", n, out)
	}
}

// 非流式响应侧：ScanResponseLosses 报出，anthropic 自家静默，无块全静默。
func TestContainerUploadResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", Content: []ir.Block{
		{Type: ir.BlockText, Text: "chart ready"}, r69Upload}}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		got := strings.Join(proto.MustInbound(name).ResponseNotes(resp), "; ")
		if !strings.Contains(got, "dropped 1 container upload block(s)") {
			t.Errorf("%s 应报块丢失：%q", name, got)
		}
	}
	if notes := proto.MustInbound("anthropic").ResponseNotes(resp); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
	resp.Content = resp.Content[:1]
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
			t.Errorf("%s 无块误报：%v", name, notes)
		}
	}
}

// 非流式编码：外族线上不出现 file_id，正文保留。
func TestContainerUploadNotLeakedIntoResponse(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "chart ready"}, r69Upload}}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustInbound(name).EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if strings.Contains(string(body), "file_secret1") || strings.Contains(string(body), "container_upload") {
				t.Errorf("非流式响应泄漏 file_id：%s", body)
			}
			if !strings.Contains(string(body), "chart ready") {
				t.Errorf("正文被误删：%s", body)
			}
		})
	}
}
