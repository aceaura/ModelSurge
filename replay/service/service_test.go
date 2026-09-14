package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/replay/schedule"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	upstreamir "github.com/aceaura/ModelSurge/upstream/ir"
)

type fakeUpstream struct {
	evaluates int
	resolves  int
	reports   int
	target    upstreamv1.ResolvedTarget
}

func (f *fakeUpstream) Health(context.Context) error { return nil }
func (f *fakeUpstream) Models(context.Context) ([]upstreamv1.ModelSummary, error) {
	return []upstreamv1.ModelSummary{{ID: "a/model"}}, nil
}
func (f *fakeUpstream) Resolve(_ context.Context, id string) (upstreamv1.ResolvedTarget, error) {
	f.resolves++
	if f.target.Protocol != "" {
		target := f.target
		target.ID = id
		return target, nil
	}
	return upstreamv1.ResolvedTarget{ID: id, Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test", APIKey: "secret"}, nil
}
func (f *fakeUpstream) Evaluate(_ context.Context, ids []string) ([]upstreamv1.CandidateEvaluation, error) {
	f.evaluates++
	return []upstreamv1.CandidateEvaluation{{ID: ids[0], Available: true}}, nil
}
func (f *fakeUpstream) Report(context.Context, upstreamv1.ResultReport) (upstreamv1.ResultResponse, error) {
	f.reports++
	return upstreamv1.ResultResponse{Applied: true}, nil
}
func (f *fakeUpstream) WebSearch(context.Context, upstreamv1.WebSearchRequest) (upstreamv1.WebSearchResponse, error) {
	return upstreamv1.WebSearchResponse{ID: "srvtoolu_test"}, nil
}

func newTestServer(t *testing.T) (*HTTPServer, *relaystore.Store, *fakeUpstream) {
	t.Helper()
	store, err := relaystore.Open(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "public", Protocol: "openai-chat", APIKey: "client-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGroup(ctx, relaystore.Group{ID: "g", UserModel: "public", PolicyType: "sticky", PolicyConfig: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMembers(ctx, "g", []string{"a/model"}); err != nil {
		t.Fatal(err)
	}
	upstream := &fakeUpstream{}
	scheduler := &schedule.Scheduler{Store: store, Upstream: upstream}
	return NewHTTPServer(&Service{Store: store, Scheduler: scheduler, Upstream: upstream}, "agent-key", "admin-key"), store, upstream
}

func TestDispatchAuthenticatesUserModelAndUsesCache(t *testing.T) {
	server, _, upstream := newTestServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	dispatch := replayv1.DispatchRequest{Model: "public", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-1"}
	var first replayv1.TargetLease
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusOK, &first)
	if first.TargetID != "a/model" || first.Credential != "secret" || upstream.evaluates != 1 || upstream.resolves != 1 {
		t.Fatalf("lease=%+v eval=%d resolve=%d", first, upstream.evaluates, upstream.resolves)
	}
	var second replayv1.TargetLease
	dispatch.RequestID = "req-2"
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusOK, &second)
	if upstream.evaluates != 1 || upstream.resolves != 2 {
		t.Fatalf("cache path eval=%d resolve=%d", upstream.evaluates, upstream.resolves)
	}

	dispatch.ClientKey = "wrong"
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusUnauthorized, nil)
}

func TestDispatchRejectsGeminiAndUnknownTargetsButAcceptsGeminiInbound(t *testing.T) {
	for _, outbound := range []string{"gemini", "unknown"} {
		t.Run(outbound, func(t *testing.T) {
			server, store, upstream := newTestServer(t)
			ctx := context.Background()
			if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "gemini-client", Protocol: "gemini", APIKey: "gemini-key", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if err := store.PutGroup(ctx, relaystore.Group{ID: "gemini-group", UserModel: "gemini-client", PolicyType: "sticky", PolicyConfig: "{}"}); err != nil {
				t.Fatal(err)
			}
			if err := store.AddMembers(ctx, "gemini-group", []string{"a/model"}); err != nil {
				t.Fatal(err)
			}
			upstream.target = upstreamv1.ResolvedTarget{Protocol: outbound, NativeModel: "native", BaseURL: "https://example.test", APIKey: "secret"}

			ts := httptest.NewServer(server.Handler())
			defer ts.Close()
			dispatch := replayv1.DispatchRequest{Model: "gemini-client", InboundProtocol: "gemini", ClientKey: "gemini-key", RequestID: "req-" + outbound}
			doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusServiceUnavailable, nil)
			if upstream.resolves != 1 {
				t.Fatalf("resolve calls=%d, want 1", upstream.resolves)
			}
		})
	}
}

func TestResultsAreIdempotentInReplayAndForwardedUpstream(t *testing.T) {
	server, store, upstream := newTestServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	report := replayv1.ResultReport{ReportID: "report-1", RequestID: "req-1", GroupID: "g", TargetID: "a/model", Outcome: "abnormal", Attempt: 1}
	var first replayv1.ResultResponse
	doJSON(t, ts.URL+replayv1.BasePath+"/results", "agent-key", report, http.StatusOK, &first)
	var second replayv1.ResultResponse
	doJSON(t, ts.URL+replayv1.BasePath+"/results", "agent-key", report, http.StatusOK, &second)
	if !first.Applied || second.Applied || upstream.reports != 2 {
		t.Fatalf("first=%v second=%v reports=%d", first.Applied, second.Applied, upstream.reports)
	}
	group, err := store.GroupForModel(context.Background(), "public")
	if err != nil || group.LastResult != "abnormal" {
		t.Fatalf("group=%+v err=%v", group, err)
	}
	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM result_reports`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestLeaseFromTargetPreservesOverrideTriState(t *testing.T) {
	zeroFloat := 0.0
	zeroInt := 0
	target := upstreamv1.ResolvedTarget{
		ID: "target", Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test",
		RequestOverrides: &upstreamir.Overrides{
			Temperature: &zeroFloat,
			TopP:        nil,
			MaxTokens:   &zeroInt,
			Thinking:    &upstreamir.ThinkingOverride{Enabled: false},
		},
	}
	lease := leaseFromTarget("request", "group", target)
	if lease.RequestOverrides == nil {
		t.Fatal("request overrides lost")
	}
	if lease.RequestOverrides.Temperature == nil || *lease.RequestOverrides.Temperature != 0 {
		t.Fatalf("temperature=%v", lease.RequestOverrides.Temperature)
	}
	if lease.RequestOverrides.TopP != nil {
		t.Fatalf("top_p=%v, want nil", lease.RequestOverrides.TopP)
	}
	if lease.RequestOverrides.MaxTokens == nil || *lease.RequestOverrides.MaxTokens != 0 {
		t.Fatalf("max_tokens=%v", lease.RequestOverrides.MaxTokens)
	}
	if lease.RequestOverrides.Thinking == nil || lease.RequestOverrides.Thinking.Enabled {
		t.Fatalf("thinking=%+v", lease.RequestOverrides.Thinking)
	}
}

func TestServiceAndAdminKeysAreIndependent(t *testing.T) {
	server, _, _ := newTestServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+replayv1.BasePath+"/models", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("service status=%v err=%v", status(resp), err)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/user-models", nil)
	req.Header.Set("X-Admin-Key", "agent-key")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin status=%v err=%v", status(resp), err)
	}
}

func doJSON(t *testing.T, url, key string, input any, want int, output any) {
	t.Helper()
	body, _ := json.Marshal(input)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status=%d want=%d", resp.StatusCode, want)
	}
	if output != nil && want >= 200 && want < 300 {
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
}

func status(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
