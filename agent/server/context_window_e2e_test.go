package server_test

import (
	"encoding/json"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/agent/agentstore"
	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/replayclient"
	"github.com/aceaura/ModelSurge/agent/server"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// 11.2 E2E：上游 400 "maximum context length" → 识别为 context_exceeded，
// 单次上游调用、单次 dispatch（不重试不换目标）、客户端 400 透传、
// 上报 outcome=context_exceeded（Upstream/Replay 侧不进熔断不污染缓存）。
func TestContextExceededEndToEnd(t *testing.T) {
	var providerCalls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer provider.Close()

	var mu sync.Mutex
	var dispatches int
	var outcomes []string
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case replayv1.BasePath + "/dispatch":
			var d replayv1.DispatchRequest
			_ = json.NewDecoder(r.Body).Decode(&d)
			mu.Lock()
			dispatches++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(replayv1.TargetLease{RequestID: d.RequestID, GroupID: "g", TargetID: "target-1", Protocol: "openai-chat", NativeModel: "native", BaseURL: provider.URL})
		case replayv1.BasePath + "/results":
			var report replayv1.ResultReport
			_ = json.NewDecoder(r.Body).Decode(&report)
			mu.Lock()
			outcomes = append(outcomes, report.Outcome)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(replayv1.ResultResponse{Applied: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer replay.Close()

	store, err := agentstore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{FirstTokenTimeoutDur: time.Second, SameAccountRetries: 3, TruncationRecoveryEnabled: true}
	h := server.New(cfg, replayclient.New(replay.URL, "service-secret", time.Second), store).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"user-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("client status=%d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "maximum context length") {
		t.Fatalf("client body=%s", w.Body.String())
	}
	if providerCalls != 1 || dispatches != 1 {
		t.Fatalf("providerCalls=%d dispatches=%d, want 1/1（不重试不换目标）", providerCalls, dispatches)
	}
	if len(outcomes) != 1 || outcomes[0] != "context_exceeded" {
		t.Fatalf("outcomes=%v, want [context_exceeded]", outcomes)
	}
}

// 11.2 E2E：调度层窗口全排除 → replay 返回 context_too_large →
// 客户端 413，且不发起任何上游调用。
func TestContextTooLargeEndToEnd(t *testing.T) {
	var providerCalls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer provider.Close()

	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == replayv1.BasePath+"/dispatch" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": replayv1.Error{Code: replayv1.CodeContextTooLarge, Message: "estimated 300000 tokens exceed all candidate context windows"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer replay.Close()

	store, err := agentstore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{FirstTokenTimeoutDur: time.Second, SameAccountRetries: 3, TruncationRecoveryEnabled: true}
	h := server.New(cfg, replayclient.New(replay.URL, "service-secret", time.Second), store).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"user-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("client status=%d, want 413", w.Code)
	}
	if !strings.Contains(w.Body.String(), "context windows") {
		t.Fatalf("client body=%s", w.Body.String())
	}
	if providerCalls != 0 {
		t.Fatalf("providerCalls=%d, want 0", providerCalls)
	}
}
