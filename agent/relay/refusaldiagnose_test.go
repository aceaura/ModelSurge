package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

const refusalNoteText = "I can't help with that."

func refusalReq() *ir.Request {
	return &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "do X"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockRefusal, Text: refusalNoteText}}},
	}}
}

// 历史里的拒绝会被并进普通文本发给无槽位的上游。这件事必须报出来，否则读者
// 会以为模型仍能看出那一轮是拒绝。真实 Caps 下 openai 两系有槽位、anthropic
// 没有，三个出站同时覆盖到分支的两侧。
func TestDiagnoseRefusalPerProtocol(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"anthropic", true},
		{"openai-chat", false},
		{"openai-responses", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			notes := Diagnose(refusalReq(), c.name, capsOf(t, c.name))
			var got string
			for _, n := range notes {
				if strings.Contains(n, "refusal") {
					got = n
				}
			}
			if c.want {
				if got == "" {
					t.Fatalf("无槽位却没报拒绝降级：%v", notes)
				}
				if !strings.Contains(got, "1 refusal(s)") {
					t.Errorf("没报出条数，读者无法判断影响面：%q", got)
				}
				return
			}
			if got != "" {
				t.Errorf("有槽位却报了降级：%q", got)
			}
		})
	}
}

// 多个拒绝块要累计计数：只报「发生了」而不报条数，读者无法判断影响面。
func TestDiagnoseRefusalCountsAll(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockRefusal, Text: "a"},
			{Type: ir.BlockRefusal, Text: "b"},
		}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockRefusal, Text: "c"}}},
	}}
	notes := Diagnose(req, "anthropic", capsOf(t, "anthropic"))
	var got string
	for _, n := range notes {
		if strings.Contains(n, "refusal") {
			got = n
		}
	}
	if !strings.Contains(got, "3 refusal(s)") {
		t.Errorf("拒绝条数没累计：%q", got)
	}
}

// 无拒绝块不得留下这条说明：恒真的诊断等于没有诊断。
func TestDiagnoseNoRefusalNoNote(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
	}}
	for _, n := range Diagnose(req, "anthropic", capsOf(t, "anthropic")) {
		if strings.Contains(n, "refusal") {
			t.Errorf("无拒绝块却报了降级：%q", n)
		}
	}
}

// 「上游非流式、客户端流式」这条路径上，拒绝块也得发出 text delta。
// 漏掉只会发出空的块开合，正文整条不见，而另一条（上游本就流式）路径全绿。
func TestEventsFromResponseCarriesRefusalText(t *testing.T) {
	resp := &ir.Response{
		ID: "msg_1", Model: "m",
		Content:    []ir.Block{{Type: ir.BlockRefusal, Text: refusalNoteText}},
		StopReason: ir.StopRefusal,
	}
	var text string
	var starts int
	for _, ev := range EventsFromResponse(resp) {
		switch ev.Type {
		case ir.EvBlockStart:
			starts++
			if ev.Block == nil || ev.Block.Type != ir.BlockRefusal {
				t.Errorf("块类型被改写：%+v", ev.Block)
			}
		case ir.EvTextDelta:
			text += ev.Text
		}
	}
	if starts != 1 {
		t.Errorf("块开启数 = %d, want 1", starts)
	}
	if text != refusalNoteText {
		t.Errorf("拒绝正文在这条路径上丢了：got %q want %q", text, refusalNoteText)
	}
}

// 空拒绝块不得合成 text delta：空 delta 会让下游编码器发出一个无内容帧。
func TestEventsFromResponseSkipsEmptyRefusal(t *testing.T) {
	resp := &ir.Response{
		ID: "msg_1", Model: "m",
		Content:    []ir.Block{{Type: ir.BlockRefusal}},
		StopReason: ir.StopRefusal,
	}
	for _, ev := range EventsFromResponse(resp) {
		if ev.Type == ir.EvTextDelta {
			t.Errorf("空拒绝块合成了 delta：%+v", ev)
		}
	}
}
