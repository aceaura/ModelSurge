package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R109-B4 目标是 anthropic 时外来投影引用的丢弃诊断：缺 Required 的
// encrypted_index（或 cited_text 反推不出）的引用编码侧整条丢弃，请求方向
// 没有 Notes 通道，由 Diagnose 在编码前按同一判据报出；带 Raw 的同族引用与
// 字段齐全的投影不报。
func TestDiagnoseR109AnthropicUnreplayableCitations(t *testing.T) {
	msg := func(cs ...ir.Citation) *ir.Request {
		return &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant,
			Content: []ir.Block{{Type: ir.BlockText, Text: "hello world", Citations: cs}}}}}
	}
	// 投影引用：cited_text 可反推但缺 encrypted_index——必丢，必须报。
	got := strings.Join(Diagnose(msg(ir.Citation{URL: "https://x", Start: 0, End: 5}),
		"anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(got, "dropped 1 citation(s)") {
		t.Fatalf("anthropic 目标应报投影引用丢弃：%q", got)
	}
	// 带 encrypted_index 的投影可落 web_search_result_location，不报。
	if notes := Diagnose(msg(ir.Citation{URL: "https://x", Start: 0, End: 5, EncryptedIndex: "e"}),
		"anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Fatalf("字段齐全的投影误报：%v", notes)
	}
	// 带 Raw 的同族引用原文回放，不报。
	if notes := Diagnose(msg(ir.Citation{Raw: []byte(`{"type":"web_search_result_location"}`)}),
		"anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Fatalf("Raw 引用误报：%v", notes)
	}
	// 同一条投影引用打到外族：走既有的不可移植/无槽位注记，
	// 不出 anthropic 专属判据的措辞。
	for _, name := range []string{"openai-chat", "openai-responses"} {
		got := strings.Join(Diagnose(msg(ir.Citation{URL: "https://x", Start: 0, End: 5}),
			name, capsOf(t, name)), "; ")
		if strings.Contains(got, "encrypted_index") {
			t.Fatalf("%s 不应出 anthropic 专属注记：%q", name, got)
		}
	}
}

// R109-A2 非流式转流式：EventsFromResponse 首帧携带上游 Created 与 metadata
// 回显，message_delta 携带服务端上下文清理回执，合成流不再比真流少字段。
func TestR109SynthStreamCarriesResponseFields(t *testing.T) {
	resp := &ir.Response{
		ID: "r1", Model: "m", Created: 1750000000,
		Metadata:             []byte(`{"trace":"x"}`),
		StopReason:           ir.StopEndTurn,
		AnthropicContextMgmt: []byte(`{"applied_edits":[]}`),
	}
	var start, delta *ir.Event
	evs := EventsFromResponse(resp)
	for i := range evs {
		switch evs[i].Type {
		case ir.EvMessageStart:
			start = &evs[i]
		case ir.EvMessageDelta:
			delta = &evs[i]
		}
	}
	if start == nil || start.Created != 1750000000 || string(start.Metadata) != `{"trace":"x"}` {
		t.Fatalf("首帧字段缺失：%+v", start)
	}
	if delta == nil || string(delta.ContextMgmt) != `{"applied_edits":[]}` {
		t.Fatalf("message_delta 回执缺失：%+v", delta)
	}

	// 零值响应不造字段。
	for _, ev := range EventsFromResponse(&ir.Response{ID: "r", StopReason: ir.StopEndTurn}) {
		if ev.Type == ir.EvMessageStart && (ev.Created != 0 || len(ev.Metadata) != 0) {
			t.Fatalf("零值被伪造：%+v", ev)
		}
		if ev.Type == ir.EvMessageDelta && len(ev.ContextMgmt) != 0 {
			t.Fatalf("零值被伪造：%+v", ev)
		}
	}
}
