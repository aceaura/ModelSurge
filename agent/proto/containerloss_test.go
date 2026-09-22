package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/kiro"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R66：container 回显的跨族损耗注记。container 是 anthropic 专属维度，
// 外族响应没有槽位：非流式走 ScanResponseLosses（各 codec ResponseNotes），
// 流式走编码器 Notes()（SSE 注释帧收尾）。

var r66Container = &ir.Container{ID: "ctr_1", ExpiresAt: "t1",
	Skills: []ir.Skill{{SkillID: "s1", Type: "custom", Version: "v1"}}}

func TestContainerResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m", Container: r66Container}
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		got := strings.Join(proto.MustInbound(name).ResponseNotes(resp), "; ")
		if !strings.Contains(got, "dropped container info") {
			t.Errorf("%s 应报 container 丢失：%q", name, got)
		}
	}
	if notes := proto.MustInbound("anthropic").ResponseNotes(resp); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
	// 无容器全静默。
	resp.Container = nil
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
			t.Errorf("%s 无容器误报：%v", name, notes)
		}
	}
}

func TestContainerStreamEncodeDropped(t *testing.T) {
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		e := proto.MustInbound(name).NewStreamEncoder()
		if _, err := e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", Container: r66Container}); err != nil {
			t.Fatalf("%s Encode start: %v", name, err)
		}
		got := strings.Join(e.Notes(), "; ")
		if !strings.Contains(got, "dropped container info") {
			t.Errorf("%s 首帧丢 container 应报：%q", name, got)
		}
	}
	// 晚到（EvMessageDelta 携带）同样报。
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		e := proto.MustInbound(name).NewStreamEncoder()
		e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m"})
		e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Container: r66Container})
		got := strings.Join(e.Notes(), "; ")
		if !strings.Contains(got, "dropped container info") {
			t.Errorf("%s 晚到 container 应报：%q", name, got)
		}
	}
	// anthropic 自家 encoder 不报。
	e := proto.MustInbound("anthropic").NewStreamEncoder()
	e.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", Container: r66Container})
	e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn})
	if notes := e.Notes(); len(notes) != 0 {
		t.Errorf("anthropic 误报：%v", notes)
	}
	// 重复到达只报一次（Notes 排干后清空）。
	e2 := proto.MustInbound("openai-chat").NewStreamEncoder()
	e2.Encode(ir.Event{Type: ir.EvMessageStart, MessageID: "m", Model: "m", Container: r66Container})
	e2.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Container: r66Container})
	first := e2.Notes()
	if len(first) != 1 {
		t.Fatalf("重复到达应合并为一条：%v", first)
	}
	if again := e2.Notes(); len(again) != 0 {
		t.Errorf("Notes 应幂等排干：%v", again)
	}
}
