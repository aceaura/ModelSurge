package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

type thinkNotesReplay struct{ lease replayv1.TargetLease }

func (r *thinkNotesReplay) Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	return r.lease, nil
}

func (*thinkNotesReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{}, nil
}

// 换算发生在拿到 lease 之后、按目标 max_tokens 算出来，Diagnose 看不到；
// 两者必须在响应头里汇合，否则客户端看到的 budget_tokens 与它给的 effort
// 之间没有任何可追溯的说明。
func TestNotesHeaderCarriesDerivedThinkingBudget(t *testing.T) {
	var body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"native","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer up.Close()

	replay := &thinkNotesReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: "anthropic-1", Protocol: "anthropic",
		NativeModel: "native", BaseURL: up.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "m", MaxTokens: 100000,
		Thinking: &ir.ThinkingConfig{Enabled: true, Effort: ir.EffortHigh},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(body, `"budget_tokens":80000`) {
		t.Fatalf("upstream body lost the derived budget: %s", body)
	}
	notes := w.Header().Get("X-ModelSurge-Notes")
	if !strings.Contains(notes, "derived thinking budget 80000 from reasoning effort high") {
		t.Fatalf("X-ModelSurge-Notes = %q, want the derivation explained", notes)
	}
}

// 没发生换算时不该凭空多出说明（两侧都给全 = 客户端已表达意图）。
func TestNotesHeaderSilentWhenNothingDerived(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"native","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer up.Close()

	replay := &thinkNotesReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: "anthropic-1", Protocol: "anthropic",
		NativeModel: "native", BaseURL: up.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "m", MaxTokens: 100000,
		Thinking: &ir.ThinkingConfig{Enabled: true, Effort: ir.EffortHigh, BudgetTokens: 2048},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if notes := w.Header().Get("X-ModelSurge-Notes"); strings.Contains(notes, "derived") {
		t.Fatalf("X-ModelSurge-Notes = %q, want no derivation note", notes)
	}
}
