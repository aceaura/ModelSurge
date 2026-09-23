package relay

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func TestDiagnoseAssistantAudioReference(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, AudioID: "audio_secret_1", Content: []ir.Block{{Type: ir.BlockText, Text: "first"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "continue"}}},
	}}
	if notes := Diagnose(req, "openai-chat", capsOf(t, "openai-chat")); len(notes) != 0 {
		t.Errorf("Chat can replay assistant audio id: %v", notes)
	}
	for _, name := range []string{"anthropic", "openai-responses"} {
		got := strings.Join(Diagnose(req, name, capsOf(t, name)), "; ")
		if !strings.Contains(got, "dropped 1 assistant audio reference(s)") {
			t.Errorf("%s missing audio reference note: %q", name, got)
		}
		if strings.Contains(got, "audio_secret_1") {
			t.Errorf("%s note leaked audio id: %q", name, got)
		}
	}
}

func TestDiagnoseAssistantAudioReferenceCountAndRole(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, AudioID: "audio_1"},
		{Role: ir.RoleAssistant, AudioID: "audio_2"},
		{Role: ir.RoleUser, AudioID: "must_not_count"},
	}}
	got := strings.Join(Diagnose(req, "anthropic", capsOf(t, "anthropic")), "; ")
	if !strings.Contains(got, "dropped 2 assistant audio reference(s)") {
		t.Errorf("assistant references not counted correctly: %q", got)
	}
	for i := range req.Messages {
		req.Messages[i].AudioID = ""
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses"} {
		if notes := Diagnose(req, name, capsOf(t, name)); len(notes) != 0 {
			t.Errorf("%s reported absent audio references: %v", name, notes)
		}
	}
}

func TestEventsFromResponseCarriesAudioOutput(t *testing.T) {
	audio := &ir.AudioOutput{ID: "audio_1", Data: "ZGF0YQ==", ExpiresAt: 1893456000, Transcript: "hello"}
	evs := EventsFromResponse(&ir.Response{ID: "r", Model: "m", Audio: audio, StopReason: ir.StopEndTurn})
	if len(evs) == 0 || evs[0].Type != ir.EvMessageStart || evs[0].Audio != audio {
		t.Fatalf("message_start did not carry audio output: %+v", evs)
	}
	agg := ir.NewAggregator()
	for _, ev := range evs {
		agg.Feed(ev)
	}
	resp, err := agg.Finish()
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if resp.Audio == nil || *resp.Audio != *audio {
		t.Errorf("event aggregation lost audio: %+v", resp.Audio)
	}
}
