package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R90：历史消息里的 redacted_thinking 块仅 anthropic 上游能接住。外族没有承载
// 不透明推理密文的槽位，必须报出整块丢失，且注记不能抄出密文——它属会话内容，
// 会随诊断头 X-ModelSurge-Notes 回到客户端与日志。

const r90Cipher = "EmwKAhgBEgy3va3pzix/LafPsn4aDFIT2Xlxh0L5L8rLVyIwxtE3rAFBa8cwF4LHqJo="

func r90Req() *ir.Request {
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockRedactedThinking, RedactedData: r90Cipher},
			{Type: ir.BlockText, Text: "answer"},
		}},
	}}
}

func TestDiagnoseRedactedThinkingDroppedOffAnthropic(t *testing.T) {
	req := r90Req()
	for _, name := range []string{"openai-chat", "openai-responses"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped 1 redacted thinking block(s)") {
			t.Errorf("%s 应报 redacted_thinking 丢失：%q", name, got)
		}
		if strings.Contains(got, r90Cipher) {
			t.Errorf("%s 注记泄漏密文：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
}

// 与 thinking 块的签名诊断分开计数：涂抹块没有签名，不该被算进「签名形态不符」，
// 否则客户端会看到一条指向不存在签名的说明。
func TestDiagnoseRedactedThinkingNotCountedAsSignature(t *testing.T) {
	req := r90Req()
	got := strings.Join(Diagnose(req, "openai-chat", capsOf(t, "openai-chat")), "; ")
	if strings.Contains(got, "signature") {
		t.Errorf("涂抹块被算进签名诊断：%q", got)
	}
}

func TestDiagnoseRedactedThinkingCountAndAbsent(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
		{Type: ir.BlockRedactedThinking, RedactedData: r90Cipher},
		{Type: ir.BlockRedactedThinking, RedactedData: r90Cipher},
	}}}}
	got := strings.Join(Diagnose(req, "openai-chat", capsOf(t, "openai-chat")), "; ")
	if !strings.Contains(got, "dropped 2 redacted thinking block(s)") {
		t.Errorf("多块应计数：%q", got)
	}
	req.Messages[0].Content = nil
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 无块误报：%v", name, notes)
		}
	}
}
