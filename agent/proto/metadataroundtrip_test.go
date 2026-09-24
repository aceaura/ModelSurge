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

// R100 扩点：metadata 贯通。探针日志 r100_probe5.log 实测：chat 与 responses
// 入站把 metadata 对象整条扔掉（IR.Metadata=null）——哪怕里面有 user_id。它是
// 官方文档维度（16 对键值，responses 一族随响应回显），是客户端的关联数据通道；
// 整条丢掉等于关联数据再也回不来，滥用追踪也看不见藏在里面的终端用户。
//
// 修法：两族入站把 metadata 解进 IR.Metadata，user 字段与 metadata.user_id 同
// 维度、同给时 user 胜出；OpenAI 两族出站原样回吐（同族无损往返）；anthropic
// 的 metadata 只有 user_id 一个键，其余键的丢失由 relay.Diagnose 报出。

func r100cDec(t *testing.T, inbound, body string) *ir.Request {
	t.Helper()
	req, err := proto.MustInbound(inbound).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("%s DecodeRequest: %v", inbound, err)
	}
	return req
}

// 两族入站都把 metadata 解进 IR，user 字段胜出。
func TestMetadataDecodesOnBothOpenAIFamilies(t *testing.T) {
	chat := r100cDec(t, "openai-chat",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"tenant":"acme","trace_id":"t-1","user_id":"u-9"}}`)
	if chat.Metadata["tenant"] != "acme" || chat.Metadata["trace_id"] != "t-1" || chat.Metadata["user_id"] != "u-9" {
		t.Errorf("chat metadata 没进 IR：%v", chat.Metadata)
	}
	resp := r100cDec(t, "openai-responses",
		`{"model":"m","input":[{"role":"user","content":"hi"}],"metadata":{"tenant":"acme","user_id":"u-9"}}`)
	if resp.Metadata["tenant"] != "acme" || resp.Metadata["user_id"] != "u-9" {
		t.Errorf("responses metadata 没进 IR：%v", resp.Metadata)
	}

	// user 与 metadata.user_id 同给时 user 胜出：顶层字段比嵌套键更显式。
	both := r100cDec(t, "openai-chat",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"user":"u-1","metadata":{"user_id":"u-2","tenant":"acme"}}`)
	if both.Metadata["user_id"] != "u-1" || both.Metadata["tenant"] != "acme" {
		t.Errorf("user 应胜过 metadata.user_id：%v", both.Metadata)
	}
	bothResp := r100cDec(t, "openai-responses",
		`{"model":"m","input":[{"role":"user","content":"hi"}],"user":"u-1","metadata":{"user_id":"u-2","tenant":"acme"}}`)
	if bothResp.Metadata["user_id"] != "u-1" || bothResp.Metadata["tenant"] != "acme" {
		t.Errorf("responses 入站 user 应胜过 metadata.user_id：%v", bothResp.Metadata)
	}

	// 形状非法（值非字符串）当没有：那种 body 上游本来也照实 400。
	bad := r100cDec(t, "openai-chat",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"n":42}}`)
	if bad.Metadata != nil {
		t.Errorf("非法 metadata 形状不该进 IR：%v", bad.Metadata)
	}
}

// 同族回吐：chat→chat、responses→responses 全部键都在线上，user_id 同时落 user。
func TestMetadataEchoedOnSameFamilyRoundTrip(t *testing.T) {
	chat := r100cDec(t, "openai-chat",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"tenant":"acme","user_id":"u-9"}}`)
	out := r100bEnc(t, "openai-chat", chat)
	for _, want := range []string{`"tenant":"acme"`, `"user_id":"u-9"`, `"user":"u-9"`} {
		if !strings.Contains(out, want) {
			t.Errorf("chat 同族回吐丢了 %s：%s", want, out)
		}
	}

	resp := r100cDec(t, "openai-responses",
		`{"model":"m","input":[{"role":"user","content":"hi"}],"metadata":{"tenant":"acme","user_id":"u-9"}}`)
	for _, outbound := range []string{"openai-responses", "codex"} {
		out := r100bEnc(t, outbound, resp)
		for _, want := range []string{`"tenant":"acme"`, `"user_id":"u-9"`, `"user":"u-9"`} {
			if !strings.Contains(out, want) {
				t.Errorf("%s 同族回吐丢了 %s：%s", outbound, want, out)
			}
		}
	}
}

// 跨族：anthropic 的 metadata 只有 user_id 一个键，其余键不上线（丢失由诊断报出）。
func TestAnthropicKeepsOnlyUserIDFromMetadata(t *testing.T) {
	resp := r100cDec(t, "openai-responses",
		`{"model":"m","input":[{"role":"user","content":"hi"}],"metadata":{"tenant":"acme","user_id":"u-9"}}`)
	out := r100bEnc(t, "anthropic", resp)
	if !strings.Contains(out, `"user_id":"u-9"`) {
		t.Errorf("anthropic 出站丢了 user_id：%s", out)
	}
	if strings.Contains(out, "acme") || strings.Contains(out, "tenant") {
		t.Errorf("anthropic 出站把装不下的 metadata 键也写了：%s", out)
	}
}

// 没有 metadata 的请求不得造出该键（R98 纪律：缺省保持缺省）。
func TestNoMetadataNoKey(t *testing.T) {
	req := r100bUserReq()
	for _, outbound := range []string{"anthropic", "codex", "openai-chat", "openai-responses"} {
		if got := r100bEnc(t, outbound, req); strings.Contains(got, `"metadata"`) {
			t.Errorf("%s 伪造了 metadata 键：%s", outbound, got)
		}
	}
}
