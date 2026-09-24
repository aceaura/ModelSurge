package openaichat

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R106-B8：messages[].name 贯通。此前解码侧字段声明了却无人读——多方
// 参与者身份（群聊/agent 编排里区分同名角色的不同实体）进不了 IR，
// 同族往返也丢。name 只有 chat 族有槽位：同族原样带回即保真口径。

func TestMessageNameRoundTrip(t *testing.T) {
	req, err := (codec{}).DecodeRequest([]byte(
		`{"model":"g","messages":[` +
			`{"role":"user","content":"hello","name":"alice"},` +
			`{"role":"assistant","content":"hi alice","name":"assistant-1"},` +
			`{"role":"user","content":[{"type":"text","text":"multi part"}],"name":"bob"}` +
			`]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("消息数不对：%d", len(req.Messages))
	}
	if req.Messages[0].Name != "alice" || req.Messages[1].Name != "assistant-1" || req.Messages[2].Name != "bob" {
		t.Fatalf("name 没进 IR：%+v", req.Messages)
	}

	out, err := (codec{}).EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, n := range []string{`"name":"alice"`, `"name":"assistant-1"`, `"name":"bob"`} {
		if !strings.Contains(s, n) {
			t.Errorf("回写缺 %s：%s", n, s)
		}
	}
}

// 无名消息不多键：缺省语义保持不变。
func TestMessageNameAbsentStaysAbsent(t *testing.T) {
	out, err := (codec{}).EncodeRequest(&ir.Request{
		Model:    "g",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"name"`) {
		t.Errorf("无名消息发明了 name 键：%s", out)
	}
}
