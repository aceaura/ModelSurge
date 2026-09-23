package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R66：container 请求参数的跨族诊断与 EventsFromResponse 透传。

func TestDiagnoseContainerDroppedOffAnthropic(t *testing.T) {
	req := &ir.Request{Container: &ir.Container{ID: "ctr_1",
		Skills: []ir.Skill{{SkillID: "s1", Type: "custom"}}}}
	for _, name := range []string{"openai-chat", "openai-responses"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped container parameter") {
			t.Errorf("%s 应报 container 丢失：%q", name, got)
		}
	}
	if notes := Diagnose(req, "anthropic", capsOf(t, "anthropic")); len(notes) != 0 {
		t.Errorf("anthropic 自家接得住，误报：%v", notes)
	}
	// 缺席静默。
	req.Container = nil
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s 无 container 误报：%v", name, notes)
		}
	}
}

// 非流式聚合转流式（EventsFromResponse）时容器回显随首事件走。
func TestEventsFromResponseCarriesContainer(t *testing.T) {
	resp := &ir.Response{ID: "m", Model: "m",
		Container: &ir.Container{ID: "ctr_1", ExpiresAt: "t1"}}
	evs := EventsFromResponse(resp)
	if len(evs) == 0 || evs[0].Container == nil || evs[0].Container.ID != "ctr_1" {
		t.Fatalf("首事件未带 container：%+v", evs)
	}
}
