package openaichat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func TestAssistantAudioReferenceRequestRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-audio","messages":[{"role":"assistant","content":"spoken","audio":{"id":"audio_123"}},{"role":"user","content":"continue"}]}`)
	req, err := codec{}.DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[0].AudioID != "audio_123" {
		t.Fatalf("assistant audio id lost: %+v", req.Messages)
	}
	clone := req.Clone()
	if clone.Messages[0].AudioID != "audio_123" {
		t.Fatalf("Clone lost audio id: %+v", clone.Messages[0])
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var wire request
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal encoded request: %v", err)
	}
	if len(wire.Messages) != 2 {
		t.Fatalf("messages = %d: %s", len(wire.Messages), out)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(wire.Messages[0].Audio, &got); err != nil {
		t.Fatalf("audio: %v", err)
	}
	if string(got["id"]) != `"audio_123"` || len(got) != 1 {
		t.Errorf("request must replay id only: %s", wire.Messages[0].Audio)
	}
}

func TestAssistantAudioReferenceSurvivesNormalization(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "first"}}},
		{Role: ir.RoleAssistant, AudioID: "audio_2"},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "continue"}}},
	}}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var wire request
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(wire.Messages) != 3 {
		t.Fatalf("adjacent assistant messages were merged: %s", out)
	}
	if string(wire.Messages[1].Audio) != `{"id":"audio_2"}` {
		t.Errorf("audio-only message lost id: %s", out)
	}
	if len(wire.Messages[1].Content) != 0 {
		t.Errorf("audio-only message gained fabricated content: %s", out)
	}
}

func TestAssistantAudioReferenceAbsentAndNull(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":"a","audio":null},{"role":"assistant","content":"b"}]}`)
	req, err := codec{}.DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	for i, m := range req.Messages {
		if m.AudioID != "" {
			t.Errorf("message %d audio id = %q", i, m.AudioID)
		}
	}
	out, err := codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if strings.Contains(string(out), `"audio"`) {
		t.Errorf("empty audio reference emitted: %s", out)
	}
}

func TestAudioOutputResponseRoundTrip(t *testing.T) {
	body := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-audio","choices":[{"index":0,"message":{"role":"assistant","content":"caption","audio":{"id":"audio_123","data":"UklGRg==","expires_at":1893456000,"transcript":"hello"}},"finish_reason":"stop"}]}`)
	resp, err := codec{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Audio == nil || resp.Audio.ID != "audio_123" || resp.Audio.Data != "UklGRg==" ||
		resp.Audio.ExpiresAt != 1893456000 || resp.Audio.Transcript != "hello" {
		t.Fatalf("audio output lost: %+v", resp.Audio)
	}
	out, err := codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	for _, want := range []string{`"id":"audio_123"`, `"data":"UklGRg=="`, `"expires_at":1893456000`, `"transcript":"hello"`, `"content":"caption"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("encoded response missing %s: %s", want, out)
		}
	}
	back, err := codec{}.DecodeResponse(out)
	if err != nil {
		t.Fatalf("DecodeResponse(round trip): %v", err)
	}
	if back.Audio == nil || *back.Audio != *resp.Audio {
		t.Errorf("round trip drift: got %+v want %+v", back.Audio, resp.Audio)
	}
}

func TestAudioOutputResponseAbsent(t *testing.T) {
	body := []byte(`{"id":"chatcmpl_1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"text","audio":null},"finish_reason":"stop"}]}`)
	resp, err := codec{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Audio != nil {
		t.Fatalf("null audio became value: %+v", resp.Audio)
	}
	out, err := codec{}.EncodeResponse(&ir.Response{ID: "r", Model: "m", StopReason: ir.StopEndTurn})
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if strings.Contains(string(out), `"audio"`) {
		t.Errorf("nil audio emitted: %s", out)
	}
}
