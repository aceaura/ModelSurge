package server_test

import (
	"context"
	"encoding/json"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/agent/agentstore"
	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/replayclient"
	"github.com/aceaura/ModelSurge/agent/server"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func TestRemoteTransientFailureRetriesSameTargetBeforeRedispatch(t *testing.T) {
	var providerCalls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		if providerCalls == 1 {
			http.Error(w, "temporary", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer provider.Close()

	var dispatches int
	var tried [][]string
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case replayv1.BasePath + "/dispatch":
			var d replayv1.DispatchRequest
			_ = json.NewDecoder(r.Body).Decode(&d)
			dispatches++
			tried = append(tried, append([]string(nil), d.TriedIDs...))
			_ = json.NewEncoder(w).Encode(replayv1.TargetLease{RequestID: d.RequestID, GroupID: "g", TargetID: "target-1", Protocol: "openai-chat", NativeModel: "native", BaseURL: provider.URL})
		case replayv1.BasePath + "/results":
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
	cfg := &config.Config{FirstTokenTimeoutDur: time.Second, SameAccountRetries: 1, TruncationRecoveryEnabled: true}
	h := server.New(cfg, replayclient.New(replay.URL, "service-secret", time.Second), store).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"user-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if providerCalls != 2 || dispatches != 2 || len(tried) != 2 || len(tried[0]) != 0 || len(tried[1]) != 0 {
		t.Fatalf("providerCalls=%d dispatches=%d tried=%v", providerCalls, dispatches, tried)
	}
}

func TestRemoteDoesNotRetryAfterFirstClientBytes(t *testing.T) {
	var providerCalls int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"native\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"started\"},\"finish_reason\":null}]}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer provider.Close()

	var dispatches int
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case replayv1.BasePath + "/dispatch":
			var d replayv1.DispatchRequest
			_ = json.NewDecoder(r.Body).Decode(&d)
			dispatches++
			_ = json.NewEncoder(w).Encode(replayv1.TargetLease{RequestID: d.RequestID, GroupID: "g", TargetID: "target-1", Protocol: "openai-chat", NativeModel: "native", BaseURL: provider.URL})
		case replayv1.BasePath + "/results":
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
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"user-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "started") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if providerCalls != 1 || dispatches != 1 {
		t.Fatalf("providerCalls=%d dispatches=%d", providerCalls, dispatches)
	}
}

func TestRemoteAddsTriedIDOnlyAfterSameTargetRetries(t *testing.T) {
	var firstCalls, secondCalls int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		http.Error(w, "temporary", http.StatusBadGateway)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"fallback"},"finish_reason":"stop"}]}`))
	}))
	defer second.Close()

	var tried [][]string
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case replayv1.BasePath + "/dispatch":
			var d replayv1.DispatchRequest
			_ = json.NewDecoder(r.Body).Decode(&d)
			tried = append(tried, append([]string(nil), d.TriedIDs...))
			lease := replayv1.TargetLease{RequestID: d.RequestID, GroupID: "g", Protocol: "openai-chat", NativeModel: "native"}
			if len(d.TriedIDs) == 0 {
				lease.TargetID, lease.BaseURL = "target-1", first.URL
			} else {
				lease.TargetID, lease.BaseURL = "target-2", second.URL
			}
			_ = json.NewEncoder(w).Encode(lease)
		case replayv1.BasePath + "/results":
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
	cfg := &config.Config{FirstTokenTimeoutDur: time.Second, SameAccountRetries: 1, TruncationRecoveryEnabled: true}
	h := server.New(cfg, replayclient.New(replay.URL, "service-secret", time.Second), store).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"user-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fallback") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if firstCalls != 2 || secondCalls != 1 || len(tried) != 3 || len(tried[0]) != 0 || len(tried[1]) != 0 || len(tried[2]) != 1 || tried[2][0] != "target-1" {
		t.Fatalf("firstCalls=%d secondCalls=%d tried=%v", firstCalls, secondCalls, tried)
	}
}

func TestClientAgentReplayProviderCrossProtocol(t *testing.T) {
	var providerModel, providerKey string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerKey = r.Header.Get("x-api-key")
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		providerModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"native-claude","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	defer provider.Close()

	var dispatchKey string
	results := make(chan replayv1.ResultReport, 1)
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-secret" {
			http.Error(w, "bad service auth", 401)
			return
		}
		switch r.URL.Path {
		case replayv1.BasePath + "/dispatch":
			var d replayv1.DispatchRequest
			_ = json.NewDecoder(r.Body).Decode(&d)
			dispatchKey = d.ClientKey
			_ = json.NewEncoder(w).Encode(replayv1.TargetLease{RequestID: d.RequestID, GroupID: "g", TargetID: "target-1", Protocol: "anthropic", NativeModel: "native-claude", BaseURL: provider.URL, Credential: "provider-secret"})
		case replayv1.BasePath + "/results":
			var result replayv1.ResultReport
			_ = json.NewDecoder(r.Body).Decode(&result)
			results <- result
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
	cfg := &config.Config{FirstTokenTimeoutDur: time.Second, AccessLogEnabled: false, TruncationRecoveryEnabled: true}
	h := server.New(cfg, replayclient.New(replay.URL, "service-secret", time.Second), store).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"user-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if dispatchKey != "client-secret" {
		t.Fatalf("dispatch client key=%q", dispatchKey)
	}
	if providerModel != "native-claude" || providerKey != "provider-secret" {
		t.Fatalf("provider model=%q key=%q", providerModel, providerKey)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("response=%s", w.Body.String())
	}
	select {
	case result := <-results:
		if result.TargetID != "target-1" || result.Outcome != "normal" {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("missing result report")
	}
	var count int
	if err := store.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM request_log WHERE target_id='target-1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("log count=%d err=%v", count, err)
	}
}
