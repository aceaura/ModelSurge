package kiro

import (
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 上游给了真签名：来源标成 kiro，才能在下一轮回传给 kiro 上游。
func TestDecoder_RealSignatureCarriesKiroOrigin(t *testing.T) {
	sig := findSig(t, driveDecoder(t, `{"text":"想"}`, `{"signature":"realsig"}`, `{"contextUsagePercentage":1}`))
	if sig.Text != "realsig" {
		t.Errorf("签名 = %q, want realsig", sig.Text)
	}
	if sig.SignatureFrom != Name {
		t.Errorf("来源 = %q, want %q", sig.SignatureFrom, Name)
	}
}

// 上游没给签名时补的占位签名必须标成 synthetic：标成 kiro 会让它被回传给
// 上游、也会被写进客户端的原生签名位冒充真签名，两边都必被拒。
func TestDecoder_FakeSignatureIsSynthetic(t *testing.T) {
	sig := findSig(t, driveDecoder(t, `{"text":"想"}`, `{"content":"答"}`, `{"contextUsagePercentage":1}`))
	if sig.SignatureFrom != ir.SigSynthetic {
		t.Errorf("占位签名来源 = %q, want %q", sig.SignatureFrom, ir.SigSynthetic)
	}
	// 合成来源与任何协议名都不相等，因此过不了任何协议的同族门控。
	th := &ir.Thinking{Text: "想", Signature: sig.Text, SignatureFrom: sig.SignatureFrom}
	for _, p := range []string{"kiro", "anthropic", "gemini", "openai-responses", "openai-chat"} {
		if th.SignatureGenuineFor(p) {
			t.Errorf("占位签名在 %s 通道上被当成真签名", p)
		}
	}
}

func findSig(t *testing.T, evs []ir.Event) ir.Event {
	t.Helper()
	for _, ev := range evs {
		if ev.Type == ir.EvSigDelta {
			return ev
		}
	}
	t.Fatalf("没有 EvSigDelta：%+v", evs)
	return ir.Event{}
}
