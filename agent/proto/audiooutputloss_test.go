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

var r70Audio = &ir.AudioOutput{
	ID: "audio_secret_id", Data: "audio_secret_data", ExpiresAt: 1893456000, Transcript: "secret transcript",
}

func TestAudioOutputResponseNotes(t *testing.T) {
	resp := &ir.Response{ID: "r", Model: "m", Audio: r70Audio}
	if notes := proto.MustInbound("openai-chat").ResponseNotes(resp); len(notes) != 0 {
		t.Errorf("Chat non-stream can carry audio output: %v", notes)
	}
	for _, name := range []string{"anthropic", "openai-responses", "gemini", "kiro"} {
		t.Run(name, func(t *testing.T) {
			got := strings.Join(proto.MustInbound(name).ResponseNotes(resp), "; ")
			if !strings.Contains(got, "dropped model audio output") {
				t.Errorf("missing audio loss note: %q", got)
			}
			for _, secret := range []string{r70Audio.ID, r70Audio.Data, r70Audio.Transcript} {
				if strings.Contains(got, secret) {
					t.Errorf("note leaked audio value %q: %q", secret, got)
				}
			}
		})
	}
	resp.Audio = nil
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini", "kiro"} {
		if notes := proto.MustInbound(name).ResponseNotes(resp); len(notes) != 0 {
			t.Errorf("%s reported absent audio: %v", name, notes)
		}
	}
}

func TestAudioOutputForeignNonStreamDoesNotLeak(t *testing.T) {
	resp := &ir.Response{ID: "r", Model: "m", StopReason: ir.StopEndTurn, Audio: r70Audio,
		Content: []ir.Block{{Type: ir.BlockText, Text: "caption"}}}
	for _, name := range []string{"anthropic", "openai-responses", "gemini", "kiro"} {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustInbound(name).EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			s := string(body)
			for _, secret := range []string{r70Audio.ID, r70Audio.Data, r70Audio.Transcript} {
				if strings.Contains(s, secret) {
					t.Errorf("response leaked audio value %q: %s", secret, s)
				}
			}
			if !strings.Contains(s, "caption") {
				t.Errorf("text was lost with audio: %s", s)
			}
		})
	}
}

func TestAudioOutputStreamDroppedWithNote(t *testing.T) {
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "r", Model: "m", Audio: r70Audio},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "caption"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	}
	for _, name := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		t.Run(name, func(t *testing.T) {
			e := proto.MustInbound(name).NewStreamEncoder()
			var wire strings.Builder
			for _, ev := range events {
				frames, err := e.Encode(ev)
				if err != nil {
					t.Fatalf("Encode(%s): %v", ev.Type, err)
				}
				for _, frame := range frames {
					wire.Write(frame)
				}
			}
			for _, secret := range []string{r70Audio.ID, r70Audio.Data, r70Audio.Transcript} {
				if strings.Contains(wire.String(), secret) {
					t.Errorf("stream leaked audio value %q: %s", secret, wire.String())
				}
			}
			if !strings.Contains(wire.String(), "caption") {
				t.Errorf("text was lost with audio: %s", wire.String())
			}
			got := strings.Join(e.Notes(), "; ")
			if !strings.Contains(got, "dropped model audio output") {
				t.Errorf("missing stream loss note: %q", got)
			}
			if again := e.Notes(); len(again) != 0 {
				t.Errorf("Notes did not drain: %v", again)
			}
		})
	}
}
