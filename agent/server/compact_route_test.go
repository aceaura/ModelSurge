package server_test

import (
	"encoding/json"
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
	"github.com/aceaura/ModelSurge/upstream/dialect"
)

// 显式压缩路由（3.1）：三条 compact 路径均按 openai-responses 入口解码转发，
// 上游 /v1/responses 收到请求并成功返回（Compact 标记不改变数据面行为）。
func TestCompactRoutesForwardAsResponses(t *testing.T) {
	var providerCalls int
	var upstreamPaths []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"native","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summarized"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer provider.Close()

	var dispatchModels []string
	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case replayv1.BasePath + "/dispatch":
			var d replayv1.DispatchRequest
			_ = json.NewDecoder(r.Body).Decode(&d)
			dispatchModels = append(dispatchModels, d.Model)
			_ = json.NewEncoder(w).Encode(replayv1.TargetLease{RequestID: d.RequestID, GroupID: "g", TargetID: "target-1", Protocol: "openai-responses", NativeModel: "native", BaseURL: provider.URL})
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
	cfg := &config.Config{FirstTokenTimeoutDur: time.Second}
	h := server.New(cfg, replayclient.New(replay.URL, "service-secret", time.Second), store).Handler()

	for _, path := range []string{"/v1/responses/compact", "/openai/v1/responses/compact", "/responses/compact"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"user-model","input":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer client-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "summarized") {
			t.Fatalf("%s: status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	if providerCalls != 3 || len(dispatchModels) != 3 {
		t.Fatalf("providerCalls=%d dispatches=%v, want 3/3", providerCalls, dispatchModels)
	}
	for _, p := range upstreamPaths {
		if p != "/v1/responses" {
			t.Fatalf("upstream path=%s, want /v1/responses", p)
		}
	}
}
