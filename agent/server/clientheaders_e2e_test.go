// clientheaders_e2e_test.go 客户端 wire 头（anthropic-beta）经 server 入口到上游的
// 端到端转交。relay 层的测试是自己构造 ctx 的，覆盖不到 server 有没有把 r.Header
// 交给 relay.WithClientHeaders——两个入口（handleChat / handleGemini）各测一次。
package server_test

import (
	"encoding/json"
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
	"github.com/aceaura/ModelSurge/upstream/dialect"
)

// betaSink 假上游 + 假 Replay：记录 anthropic 上游收到的 anthropic-beta。
type betaSink struct {
	mu      sync.Mutex
	betas   []string
	handler http.Handler
}

func newBetaSink(t *testing.T) *betaSink {
	t.Helper()
	s := &betaSink{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.betas = append(s.betas, r.Header.Get("anthropic-beta"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(provider.Close)

	replay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case replayv1.BasePath + "/dispatch":
			var d replayv1.DispatchRequest
			_ = json.NewDecoder(r.Body).Decode(&d)
			_ = json.NewEncoder(w).Encode(replayv1.TargetLease{
				RequestID: d.RequestID, GroupID: "g", TargetID: "t1",
				Protocol: "anthropic", NativeModel: "claude", BaseURL: provider.URL, Credential: "sk-up",
			})
		case replayv1.BasePath + "/results":
			_ = json.NewEncoder(w).Encode(replayv1.ResultResponse{Applied: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(replay.Close)

	store, err := agentstore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &config.Config{FirstTokenTimeoutDur: 5 * time.Second}
	s.handler = server.New(cfg, replayclient.New(replay.URL, "service-secret", time.Second), store).Handler()
	return s
}

func (s *betaSink) last(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.betas) == 0 {
		t.Fatal("上游没被调用")
	}
	return s.betas[len(s.betas)-1]
}

func TestAnthropicBetaReachesUpstreamThroughServer(t *testing.T) {
	for _, c := range []struct {
		name, path, body string
	}{
		{"anthropic 入口", "/v1/messages",
			`{"model":"claude","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
		{"gemini 入口", "/v1beta/models/claude:generateContent",
			`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newBetaSink(t)
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer client-secret")
			req.Header.Set("anthropic-beta", "context-1m-2025-08-07,oauth-2025-04-20")
			w := httptest.NewRecorder()
			s.handler.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if got := s.last(t); got != "context-1m-2025-08-07" {
				t.Fatalf("上游收到的 anthropic-beta=%q, want %q", got, "context-1m-2025-08-07")
			}
		})
	}
}
