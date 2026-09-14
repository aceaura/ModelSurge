package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"io"
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
	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/replay/schedule"
	replayservice "github.com/aceaura/ModelSurge/replay/service"
	"github.com/aceaura/ModelSurge/replay/upstreamclient"
	"github.com/aceaura/ModelSurge/upstream/account"
	upstreamservice "github.com/aceaura/ModelSurge/upstream/service"
	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

func TestRealFourHopHTTPPipeline(t *testing.T) {
	ctx := context.Background()
	provider := newMockProvider(t)
	defer provider.Close()

	upStore, err := upstreamstore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer upStore.Close()
	if err := upStore.Accounts.InsertAccount(&account.Account{
		Name: "mock", Type: account.TypeAPIKey, Enabled: true, Protocol: "anthropic",
		BaseURL: provider.URL, APIKey: "provider-secret", Models: map[string]string{"chat": "native-claude"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := upStore.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	manager, err := account.NewManager(upStore.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	upstreamHTTP := httptest.NewServer(upstreamservice.NewHTTPServer(upstreamservice.NewService(upStore, manager), "upstream-secret").Handler())
	defer upstreamHTTP.Close()

	replayStore, err := relaystore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer replayStore.Close()
	if err := replayStore.PutUserModel(ctx, relaystore.UserModel{Name: "chat", Protocol: "auto", APIKey: "client-secret", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := replayStore.PutGroup(ctx, relaystore.Group{ID: "group-chat", UserModel: "chat", PolicyType: "preset", PolicyConfig: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := replayStore.AddMembers(ctx, "group-chat", []string{"mock/chat"}); err != nil {
		t.Fatal(err)
	}
	upClient := upstreamclient.New(upstreamHTTP.URL, "upstream-secret", time.Second)
	scheduler := &schedule.Scheduler{Store: replayStore, Upstream: upClient, CacheTTL: time.Hour}
	replayHTTP := httptest.NewServer(replayservice.NewHTTPServer(&replayservice.Service{Store: replayStore, Scheduler: scheduler, Upstream: upClient}, "replay-secret", "admin-secret").Handler())
	defer replayHTTP.Close()

	agentStore, err := agentstore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer agentStore.Close()
	agentHandler := server.New(&config.Config{FirstTokenTimeoutDur: time.Second, TruncationRecoveryEnabled: true}, replayclient.New(replayHTTP.URL, "replay-secret", time.Second), agentStore).Handler()
	agentHTTP := httptest.NewServer(agentHandler)
	defer agentHTTP.Close()

	t.Run("non-stream cross-protocol and cache miss", func(t *testing.T) {
		body := post(t, agentHTTP.URL+"/v1/chat/completions", `{"model":"chat","messages":[{"role":"user","content":"hello"}]}`, "client-secret")
		if !strings.Contains(body, "mock hello") {
			t.Fatalf("response=%s", body)
		}
		assertCounts(t, provider, upStore, replayStore, agentStore, 1, 1)
	})

	t.Run("stream same-protocol and cache hit", func(t *testing.T) {
		body := post(t, agentHTTP.URL+"/v1/messages", `{"model":"chat","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`, "client-secret")
		if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "mock hello") {
			t.Fatalf("stream=%s", body)
		}
		assertCounts(t, provider, upStore, replayStore, agentStore, 2, 2)
		provider.mu.Lock()
		defer provider.mu.Unlock()
		if provider.models[len(provider.models)-1] != "native-claude" || provider.keys[len(provider.keys)-1] != "provider-secret" {
			t.Fatalf("provider models=%v keys=%v", provider.models, provider.keys)
		}
	})

	t.Run("gemini non-stream inbound to anthropic outbound", func(t *testing.T) {
		body := post(t, agentHTTP.URL+"/v1beta/models/chat:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, "client-secret")
		if !strings.Contains(body, `"candidates"`) || !strings.Contains(body, "mock hello") {
			t.Fatalf("gemini response=%s", body)
		}
		assertCounts(t, provider, upStore, replayStore, agentStore, 3, 3)
	})

	t.Run("gemini stream inbound to anthropic outbound", func(t *testing.T) {
		body := post(t, agentHTTP.URL+"/v1beta/models/chat:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, "client-secret")
		if !strings.Contains(body, "data:") || !strings.Contains(body, `"candidates"`) || !strings.Contains(body, "mock hello") {
			t.Fatalf("gemini stream=%s", body)
		}
		assertCounts(t, provider, upStore, replayStore, agentStore, 4, 4)
	})

	t.Run("abnormal result invalidates cached target", func(t *testing.T) {
		provider.failNext(2)
		body := post(t, agentHTTP.URL+"/v1/chat/completions", `{"model":"chat","messages":[{"role":"user","content":"fail"}]}`, "client-secret")
		if !strings.Contains(body, "mock failure") {
			t.Fatalf("failure response=%s", body)
		}
		var result string
		if err := replayStore.DB.QueryRow(`SELECT last_result FROM target_cache WHERE group_id='group-chat'`).Scan(&result); err != nil || result != "abnormal" {
			t.Fatalf("cache result=%q err=%v", result, err)
		}
		var failures int
		if err := upStore.DB.QueryRow(`SELECT failures FROM model_state WHERE upstream_model_id='mock/chat'`).Scan(&failures); err != nil || failures != 1 {
			t.Fatalf("failures=%d err=%v", failures, err)
		}
	})
}

type mockProvider struct {
	*httptest.Server
	mu       sync.Mutex
	requests int
	models   []string
	keys     []string
	failures int
}

func newMockProvider(t *testing.T) *mockProvider {
	p := &mockProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		p.mu.Lock()
		p.requests++
		p.models = append(p.models, req.Model)
		p.keys = append(p.keys, r.Header.Get("x-api-key"))
		fail := p.failures > 0
		if fail {
			p.failures--
		}
		p.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("mock failure"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_real\",\"model\":%q,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n", req.Model),
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"mock hello\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	return p
}

func (p *mockProvider) failNext(n int) {
	p.mu.Lock()
	p.failures += n
	p.mu.Unlock()
}

func post(t *testing.T, url, body, key string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertCounts(t *testing.T, provider *mockProvider, upStore *upstreamstore.Store, replayStore *relaystore.Store, agentStore *agentstore.Store, wantProvider, wantReports int) {
	t.Helper()
	provider.mu.Lock()
	gotProvider := provider.requests
	provider.mu.Unlock()
	if gotProvider != wantProvider {
		t.Fatalf("provider requests=%d want=%d", gotProvider, wantProvider)
	}
	deadline := time.Now().Add(time.Second)
	for {
		var replayReports, upstreamReports, agentLogs int
		_ = replayStore.DB.QueryRow(`SELECT count(*) FROM result_reports`).Scan(&replayReports)
		_ = upStore.DB.QueryRow(`SELECT count(*) FROM result_reports`).Scan(&upstreamReports)
		_ = agentStore.DB.QueryRow(`SELECT count(*) FROM request_log`).Scan(&agentLogs)
		if replayReports == wantReports && upstreamReports == wantReports && agentLogs == wantReports {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reports replay=%d upstream=%d agent=%d want=%d", replayReports, upstreamReports, agentLogs, wantReports)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
