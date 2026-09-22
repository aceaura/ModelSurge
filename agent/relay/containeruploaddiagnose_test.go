package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R69：历史消息里的 container_upload 块仅 anthropic 上游能接住；外族必须
// 报出整块丢失，且注记不能抄出 file_id。
func TestDiagnoseContainerUploadDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockText, Text: "run this"},
		{Type: ir.BlockContainerUpload, ContainerUpload: &ir.ContainerUploadRef{FileID: "file_secret1"}},
	}}}}
	for _, name := range []string{"openai-chat", "openai-responses", "kiro"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped 1 container upload block(s)") {
			t.Errorf("%s 应报 container_upload 丢失：%q", name, got)
		}
		if strings.Contains(got, "file_secret1") {
			t.Errorf("%s 注记泄漏 file_id：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

func TestDiagnoseContainerUploadCountAndAbsent(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
		{Type: ir.BlockContainerUpload, ContainerUpload: &ir.ContainerUploadRef{FileID: "file_1"}},
		{Type: ir.BlockContainerUpload, ContainerUpload: &ir.ContainerUploadRef{FileID: "file_2"}},
	}}}}
	got := strings.Join(Diagnose(req, "kiro", capsOf(t, "kiro")), "; ")
	if !strings.Contains(got, "dropped 2 container upload block(s)") {
		t.Errorf("多块应计数：%q", got)
	}
	req.Messages[0].Content = nil
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "kiro"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 无块误报：%v", name, notes)
		}
	}
}
