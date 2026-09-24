package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R100 扩点请求侧诊断：输出约束与 metadata 三态。
//
//   - 非 JSON 的 responseMimeType：Gemini 独有维度，四个出站（gemini 只入不出）
//     没有一个接得住，恒报。客户端约束了输出类型，模型却按默认格式作答——
//     解析方拿到的可能是散文。
//   - chat 目标的 modalities 值集只有 text/audio：gemini 归一来的 image 被
//     编码器滤掉（写出去是必 400），滤掉这件事必须报。
//   - metadata 里 user_id 之外的键：anthropic 的 metadata 只有 user_id 一个
//     槽位，其余键到不了上游，客户端的关联数据不会随响应回来。

func r100eNotes(t *testing.T, req *ir.Request, outbound string) string {
	t.Helper()
	return strings.Join(Diagnose(req, outbound, capsOf(t, outbound)), "; ")
}

func r100eReq() *ir.Request {
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
}

// responseMimeType 四个出站恒报；缺省静默。
func TestDiagnoseReportsResponseMimeType(t *testing.T) {
	req := r100eReq()
	req.ResponseMimeType = "text/x.enum"
	for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
		got := r100eNotes(t, req, outbound)
		if !strings.Contains(got, `"text/x.enum"`) {
			t.Errorf("%s 没报出被丢的 MIME 值：%q", outbound, got)
		}
	}
	if got := r100eNotes(t, r100eReq(), "anthropic"); strings.Contains(got, "mime type") {
		t.Errorf("没给 MIME 约束却误报：%q", got)
	}
}

// chat 目标：image 模态被滤要报，text/audio 全量过线不报。
func TestDiagnoseReportsUnsupportedModalitiesOnChat(t *testing.T) {
	req := r100eReq()
	req.Modalities = []string{"text", "image"}
	got := r100eNotes(t, req, "openai-chat")
	if !strings.Contains(got, "dropped modalities image") {
		t.Errorf("chat 目标没报出被滤的 image 模态：%q", got)
	}

	req.Modalities = []string{"text", "audio"}
	if got := r100eNotes(t, req, "openai-chat"); strings.Contains(got, "modalities") {
		t.Errorf("text/audio 在 chat 上全量过线，不该报：%q", got)
	}

	// 非 chat 目标照旧整条报（responses 全系无槽位）。
	req.Modalities = []string{"text", "image"}
	if got := r100eNotes(t, req, "openai-responses"); !strings.Contains(got, "modalities") {
		t.Errorf("responses 目标没报出 modalities 丢失：%q", got)
	}
}

// anthropic 目标：user_id 之外的 metadata 键要报，只带 user_id 不报，
// OpenAI 两族目标全量回吐不报。
func TestDiagnoseReportsNonUserIDMetadataKeysOnAnthropic(t *testing.T) {
	req := r100eReq()
	req.Metadata = map[string]string{"user_id": "u-9", "tenant": "acme", "trace_id": "t-1"}
	got := r100eNotes(t, req, "anthropic")
	if !strings.Contains(got, "dropped 2 metadata key(s)") {
		t.Errorf("anthropic 没报出被丢的两个 metadata 键：%q", got)
	}
	if strings.Contains(got, "user_id\":\"u-9\"") {
		t.Errorf("user_id 是能到上游的键，不该算进丢失：%q", got)
	}

	req.Metadata = map[string]string{"user_id": "u-9"}
	if got := r100eNotes(t, req, "anthropic"); strings.Contains(got, "metadata key") {
		t.Errorf("只带 user_id 时不该报 metadata 键丢失：%q", got)
	}

	req.Metadata = map[string]string{"tenant": "acme"}
	for _, outbound := range []string{"openai-chat", "openai-responses", "codex"} {
		if got := r100eNotes(t, req, outbound); strings.Contains(got, "metadata key") {
			t.Errorf("%s 全量接得住 metadata，不该报：%q", outbound, got)
		}
	}
}
