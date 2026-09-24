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

// R100 扩点：伪造缺省值。探针日志 r100_probe5.log 实测：responses/codex 一族
// 出站对「客户端没给」的两个字段照写不误——store 恒写 false、instructions 恒写
// 空串。两者都不是无害的占位：
//
//   - store 的官方缺省是 true。伪造 false 之后响应不再落库，
//     previous_response_id 会话链与事后拉取一起断掉，而客户端根本没提过这个
//     字段——它不可能知道链是自己「没给」还是被代理「替它表态」断的。
//   - instructions 缺省即无系统指令。伪造空串是把「没给」改写成「给了一条空
//     指令」：对官方端点是多余的键，对按前缀计费的缓存是一次形状变化。
//
// 订阅端点（Codex 形态）确实强制这两条（instructions 字段存在、store=false），
// 所以默认值只许 codex 一侧补，官方 openai-responses 出站必须保持缺省。
// 与 R98 同一纪律：缺省的槽位在出站保持缺省，伪造 = 替客户端表态。

func r100bEnc(t *testing.T, outbound string, req *ir.Request) string {
	t.Helper()
	body, err := proto.MustOutbound(outbound).EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s EncodeRequest: %v", outbound, err)
	}
	return string(body)
}

func r100bUserReq() *ir.Request {
	return &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
}

// 缺省 store：codex 补 false，openai-responses 一个字都不许写。
func TestStoreDefaultOnlyOnCodex(t *testing.T) {
	req := r100bUserReq()
	if got := r100bEnc(t, "codex", req); !strings.Contains(got, `"store":false`) {
		t.Errorf("codex 缺省 store=false 兜底丢了：%s", got)
	}
	if got := r100bEnc(t, "openai-responses", req); strings.Contains(got, `"store"`) {
		t.Errorf("openai-responses 伪造了客户端没给的 store：%s", got)
	}
}

// 显式值两家都透传：true 是客户端的选择（要点会话链），false 同理。
func TestExplicitStorePassesThroughOnBoth(t *testing.T) {
	tr, fa := true, false
	for _, outbound := range []string{"codex", "openai-responses"} {
		req := r100bUserReq()
		req.Store = &tr
		if got := r100bEnc(t, outbound, req); !strings.Contains(got, `"store":true`) {
			t.Errorf("%s 丢了显式 store=true：%s", outbound, got)
		}
		req.Store = &fa
		if got := r100bEnc(t, outbound, req); !strings.Contains(got, `"store":false`) {
			t.Errorf("%s 丢了显式 store=false：%s", outbound, got)
		}
	}
}

// 缺省 instructions：codex 补空串，openai-responses 不出键。有 system 时两家
// 都写正文。
func TestInstructionsDefaultOnlyOnCodex(t *testing.T) {
	req := r100bUserReq()
	if got := r100bEnc(t, "codex", req); !strings.Contains(got, `"instructions":""`) {
		t.Errorf("codex 缺省 instructions 空串兜底丢了：%s", got)
	}
	if got := r100bEnc(t, "openai-responses", req); strings.Contains(got, `"instructions"`) {
		t.Errorf("openai-responses 伪造了空 instructions：%s", got)
	}

	req.System = []ir.Block{{Type: ir.BlockText, Text: "be nice"}}
	for _, outbound := range []string{"codex", "openai-responses"} {
		if got := r100bEnc(t, outbound, req); !strings.Contains(got, `"instructions":"be nice"`) {
			t.Errorf("%s 丢了 system 正文：%s", outbound, got)
		}
	}
}

// instructions 为空串的入站请求（订阅端点形态）不产生 system 块：空串不是指令。
func TestEmptyInstructionsInboundYieldsNoSystem(t *testing.T) {
	req, err := proto.MustInbound("openai-responses").DecodeRequest(
		[]byte(`{"model":"m","instructions":"","input":"hi"}`))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.System) != 0 {
		t.Errorf("空 instructions 解出了 system：%+v", req.System)
	}
}
