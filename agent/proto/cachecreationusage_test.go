package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

func TestAnthropicDecodesCacheCreationDetails(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":40,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10}}}`)
	resp, err := proto.MustOutbound("anthropic").DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	assertCacheCreationUsage(t, resp.Usage, 30, 20, 10)
	if got := resp.Usage.TotalInput(); got != 170 {
		t.Errorf("TotalInput() = %d, want 170", got)
	}
}

func TestAnthropicDerivesAggregateFromNestedCacheCreation(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10}}}`)
	resp, err := proto.MustOutbound("anthropic").DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	assertCacheCreationUsage(t, resp.Usage, 30, 20, 10)
}

func TestAnthropicStreamCacheCreationDetailsAreAuthoritative(t *testing.T) {
	got := streamUsage(t, "anthropic", [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":100,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10}}}}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"cache_creation_input_tokens":25,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":0}}}`},
		{"message_stop", `{"type":"message_stop"}`},
	})
	assertCacheCreationUsage(t, got, 25, 25, 0)
	if got.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", got.OutputTokens)
	}
}

func TestMergeNonZeroReplacesKnownCacheCreationDetailsIncludingZero(t *testing.T) {
	u := ir.Usage{
		CacheCreationTokens:       30,
		CacheCreation5mTokens:     20,
		CacheCreation1hTokens:     10,
		CacheCreationDetailsKnown: true,
	}
	u.MergeNonZero(ir.Usage{
		CacheCreationTokens:       25,
		CacheCreation5mTokens:     25,
		CacheCreation1hTokens:     0,
		CacheCreationDetailsKnown: true,
	})
	assertCacheCreationUsage(t, u, 25, 25, 0)
}

func TestCacheCreationDetailsDoNotDoubleCountTotalInput(t *testing.T) {
	u := ir.Usage{
		InputTokens:               100,
		CacheReadTokens:           40,
		CacheCreationTokens:       30,
		CacheCreation5mTokens:     20,
		CacheCreation1hTokens:     10,
		CacheCreationDetailsKnown: true,
	}
	if got := u.TotalInput(); got != 170 {
		t.Errorf("TotalInput() = %d, want 170", got)
	}
}

func TestAnthropicEncodesCacheCreationDetails(t *testing.T) {
	resp := cacheCreationResponse()
	body, err := proto.MustInbound("anthropic").EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var got struct {
		Usage struct {
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheCreation            *struct {
				Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
				Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if got.Usage.CacheCreationInputTokens != 30 || got.Usage.CacheCreation == nil {
		t.Fatalf("cache creation usage = %+v", got.Usage)
	}
	if got.Usage.CacheCreation.Ephemeral5mInputTokens != 30 || got.Usage.CacheCreation.Ephemeral1hInputTokens != 0 {
		t.Errorf("cache_creation = %+v", got.Usage.CacheCreation)
	}
	if !strings.Contains(string(body), `"ephemeral_1h_input_tokens":0`) {
		t.Errorf("known zero 1h detail was omitted: %s", body)
	}
}

func TestForeignStreamEncodersMergeStartAndDeltaUsage(t *testing.T) {
	start := ir.Usage{
		InputTokens:               100,
		CacheReadTokens:           40,
		CacheCreationTokens:       30,
		CacheCreation5mTokens:     20,
		CacheCreation1hTokens:     10,
		CacheCreationDetailsKnown: true,
	}
	delta := ir.Usage{OutputTokens: 5}
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m", Usage: &start},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &delta},
		{Type: ir.EvMessageStop},
	}
	for _, tc := range []struct {
		name  string
		wants []string
	}{
		{"openai-chat", []string{`"prompt_tokens":170`, `"completion_tokens":5`}},
		{"openai-responses", []string{`"input_tokens":170`, `"output_tokens":5`}},
		{"gemini", []string{`"promptTokenCount":170`, `"candidatesTokenCount":5`, `"totalTokenCount":175`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, notes := streamOutputAndNotes(t, tc.name, events)
			for _, want := range tc.wants {
				if !strings.Contains(out, want) {
					t.Errorf("stream missing %s:\n%s", want, out)
				}
			}
			if len(notes) != 1 || !strings.Contains(notes[0], "cache-creation TTL details") {
				t.Errorf("TTL detail loss note = %v", notes)
			}
		})
	}
}

func TestAnthropicStreamPreservesCacheCreationDetails(t *testing.T) {
	start := cacheCreationResponse().Usage
	out, notes := streamOutputAndNotes(t, "anthropic", []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "claude", Usage: &start},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 5}},
		{Type: ir.EvMessageStop},
	})
	for _, want := range []string{`"cache_creation_input_tokens":30`, `"ephemeral_5m_input_tokens":30`, `"ephemeral_1h_input_tokens":0`} {
		if !strings.Contains(out, want) {
			t.Errorf("anthropic stream missing %s:\n%s", want, out)
		}
	}
	for _, note := range notes {
		if strings.Contains(note, "cache-creation TTL details") {
			t.Errorf("native Anthropic stream reported false loss: %v", notes)
		}
	}
}

func TestCacheCreationDetailLossReportedOnlyForForeignProtocols(t *testing.T) {
	resp := cacheCreationResponse()
	for _, name := range []string{"openai-chat", "openai-responses", "gemini"} {
		notes := proto.MustInbound(name).ResponseNotes(resp)
		if !containsCacheCreationNote(notes) {
			t.Errorf("%s notes = %v", name, notes)
		}
	}
	if notes := proto.MustInbound("anthropic").ResponseNotes(resp); containsCacheCreationNote(notes) {
		t.Errorf("anthropic notes = %v", notes)
	}
}

func assertCacheCreationUsage(t *testing.T, got ir.Usage, aggregate, fiveMinute, oneHour int) {
	t.Helper()
	if got.CacheCreationTokens != aggregate || got.CacheCreation5mTokens != fiveMinute || got.CacheCreation1hTokens != oneHour || !got.CacheCreationDetailsKnown {
		t.Errorf("cache creation usage = %+v, want aggregate=%d 5m=%d 1h=%d known=true", got, aggregate, fiveMinute, oneHour)
	}
}

func cacheCreationResponse() *ir.Response {
	return &ir.Response{
		ID: "msg_1", Model: "claude", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}},
		Usage: ir.Usage{
			InputTokens:               100,
			OutputTokens:              5,
			CacheReadTokens:           40,
			CacheCreationTokens:       30,
			CacheCreation5mTokens:     30,
			CacheCreation1hTokens:     0,
			CacheCreationDetailsKnown: true,
		},
	}
}

func streamOutputAndNotes(t *testing.T, name string, events []ir.Event) (string, []string) {
	t.Helper()
	// 本文件的测试考的是 usage 的合并与渲染，一律按客户端 opt-in 建编码器：
	// Chat 的 usage 帧只在 stream_options.include_usage 时才出现。
	enc := proto.NewClientStreamEncoder(proto.MustInbound(name), &ir.Request{IncludeUsage: true})
	var out strings.Builder
	for _, ev := range events {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatalf("%s Encode(%s): %v", name, ev.Type, err)
		}
		for _, frame := range frames {
			out.Write(frame)
		}
	}
	for _, frame := range enc.Finish() {
		out.Write(frame)
	}
	return out.String(), enc.Notes()
}

func containsCacheCreationNote(notes []string) bool {
	for _, note := range notes {
		if strings.Contains(note, "cache-creation TTL details") {
			return true
		}
	}
	return false
}
