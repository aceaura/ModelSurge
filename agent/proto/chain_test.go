package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/kiro"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// Responses 会话链三维横切：previous_response_id 与 store 同协议出站原样
// 回写（链能接上）；item_reference 代理无状态解析不了，只记数供诊断。
// 其余三族出站没有这些槽位，一个字符都不许泄漏。

const chainBody = `{"model":"m","previous_response_id":"resp-r54-clue","store":true,
	"input":[{"type":"item_reference","id":"msg_old"},
		{"type":"item_reference","id":"rs_old"},
		{"type":"message","role":"user","content":"hi"}]}`

func TestChainFieldsDecodeIntoIR(t *testing.T) {
	r, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(chainBody))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if r.PreviousResponseID != "resp-r54-clue" {
		t.Errorf("PreviousResponseID=%q", r.PreviousResponseID)
	}
	if r.Store == nil || !*r.Store {
		t.Errorf("Store=%v, want explicit true", r.Store)
	}
	if r.ItemRefs != 2 {
		t.Errorf("ItemRefs=%d, want 2", r.ItemRefs)
	}
	// item_reference 的内容拿不到，但正常条目必须照常进 IR。
	if len(r.Messages) != 1 {
		t.Errorf("Messages=%d, want 1（引用条目不造消息）", len(r.Messages))
	}
}

// 同协议出站（responses/codex）：链锚点与显式 store 原样回写。
func TestChainFieldsRoundTripSameProtocol(t *testing.T) {
	r, _ := proto.MustInbound("openai-responses").DecodeRequest([]byte(chainBody))
	for _, name := range []string{"openai-responses", "codex"} {
		out, err := proto.MustOutbound(name).EncodeRequest(r)
		if err != nil {
			t.Fatalf("%s: EncodeRequest err=%v", name, err)
		}
		s := string(out)
		if !strings.Contains(s, `"previous_response_id":"resp-r54-clue"`) {
			t.Errorf("%s: 链锚点没回写: %s", name, s)
		}
		if !strings.Contains(s, `"store":true`) {
			t.Errorf("%s: 显式 store=true 被改写: %s", name, s)
		}
	}
}

// 其他三族没有会话链槽位：链锚点一个字符都不许进载荷。
func TestChainFieldsNeverLeakToOtherProtocols(t *testing.T) {
	r, _ := proto.MustInbound("openai-responses").DecodeRequest([]byte(chainBody))
	for _, name := range []string{"anthropic", "openai-chat", "kiro"} {
		out, err := proto.MustOutbound(name).EncodeRequest(r)
		if err != nil {
			t.Fatalf("%s: EncodeRequest err=%v", name, err)
		}
		if strings.Contains(string(out), "resp-r54-clue") {
			t.Errorf("%s: previous_response_id 泄漏进无此维度的协议: %s", name, out)
		}
	}
}

// 缺省语义保持：没给 store 仍写 false（codex 订阅端点硬要求），
// 没给 previous_response_id 不出键，ItemRefs 为零。
func TestChainDefaultsPreserved(t *testing.T) {
	r, err := proto.MustInbound("openai-responses").DecodeRequest(
		[]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatalf("DecodeRequest err=%v", err)
	}
	if r.Store != nil || r.PreviousResponseID != "" || r.ItemRefs != 0 {
		t.Errorf("没给却非零：Store=%v Prev=%q ItemRefs=%d", r.Store, r.PreviousResponseID, r.ItemRefs)
	}
	for _, name := range []string{"openai-responses", "codex"} {
		out, _ := proto.MustOutbound(name).EncodeRequest(r)
		s := string(out)
		if !strings.Contains(s, `"store":false`) {
			t.Errorf("%s: 缺省 store=false 兜底丢了: %s", name, s)
		}
		if strings.Contains(s, "previous_response_id") {
			t.Errorf("%s: 没给链锚点却造出该键: %s", name, s)
		}
	}
}

// 显式 store=false 与「没给」不同：显式 false 透传出去还是 false，语义等价，
// 但 IR 必须记得客户端表过态（三态指针判据，同 R50）。
func TestChainExplicitStoreFalseRemembered(t *testing.T) {
	r, _ := proto.MustInbound("openai-responses").DecodeRequest(
		[]byte(`{"model":"m","store":false,"input":"hi"}`))
	if r.Store == nil || *r.Store {
		t.Errorf("显式 store=false 应进 IR 三态：Store=%v", r.Store)
	}
}
