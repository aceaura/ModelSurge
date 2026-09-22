package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func TestAnthropicStartOnlyUsageReachesClientAndReport(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10}}}}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte(frame + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer up.Close()

	replay := &reportCaptureReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "anthropic-1", Protocol: "anthropic",
		NativeModel: "claude", BaseURL: up.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		// IncludeUsage 客户端 opt-in（stream_options.include_usage）：本测试考的是
		// usage 同时抵达客户端与记账上报，所以走 opt-in 那条路。未 opt-in 时
		// 客户端不该看到 usage 帧、但记账仍要有数——见 streamusagefidelity_test.go。
		Model: "public", MaxTokens: 64, Stream: true, IncludeUsage: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"prompt_tokens":170`, `"completion_tokens":0`, ": modelsurge-note: dropped Anthropic cache-creation TTL details"} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing %s:\n%s", want, body)
		}
	}
	if len(replay.reports) != 1 {
		t.Fatalf("reports=%d, want 1", len(replay.reports))
	}
	got := replay.reports[0].Usage
	want := replayv1.Usage{InputTokens: 100, CacheRead: 40, CacheCreation: 30}
	if got != want {
		t.Errorf("reported usage = %+v, want %+v", got, want)
	}
}
