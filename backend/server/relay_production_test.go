package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"relayd/backend/config"
	"relayd/backend/contract/upstreamv1"
	"relayd/backend/relaystore"
	"relayd/backend/schedule"
)

type adminUpstream struct{}

func (adminUpstream) Models(context.Context) ([]upstreamv1.ModelSummary, error) {
	return []upstreamv1.ModelSummary{{ID: "u/m"}}, nil
}
func (adminUpstream) Resolve(context.Context, string) (upstreamv1.ResolvedTarget, error) {
	return upstreamv1.ResolvedTarget{}, nil
}
func (adminUpstream) Evaluate(context.Context, []string) ([]upstreamv1.CandidateEvaluation, error) {
	return nil, nil
}
func (adminUpstream) Report(context.Context, upstreamv1.ResultReport) error { return nil }

func TestRelayAdminCRUDAndAuthentication(t *testing.T) {
	store, err := relaystore.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sched := &schedule.Scheduler{Store: store, Upstream: adminUpstream{}}
	s := NewRelay(&config.Config{Admin: &config.Admin{APIKey: "admin"}}, sched)
	h := s.Handler()

	put := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		r.Header.Set("X-Admin-Key", "admin")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := put("/admin/user-models/m", `{"protocol":"anthropic","api_key":"model-key","enabled":true}`); w.Code != 200 {
		t.Fatalf("user model: %d %s", w.Code, w.Body.String())
	}
	if w := put("/admin/groups/g", `{"user_model":"m","policy_type":"sticky","policy_config":"{}"}`); w.Code != 200 {
		t.Fatalf("group: %d %s", w.Code, w.Body.String())
	}
	if w := put("/admin/groups/g/members", `{"members":["u/m"]}`); w.Code != 200 {
		t.Fatalf("members: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("missing admin key status=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
	r.Header.Set("X-Admin-Key", "admin")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"u/m"`) {
		t.Fatalf("groups: %d %s", w.Code, w.Body.String())
	}
}

type authUpstream struct{ provider string }

func (a authUpstream) Models(context.Context) ([]upstreamv1.ModelSummary, error) { return nil, nil }
func (a authUpstream) Evaluate(context.Context, []string) ([]upstreamv1.CandidateEvaluation, error) {
	return []upstreamv1.CandidateEvaluation{{ID: "u/m", Available: true}}, nil
}
func (a authUpstream) Resolve(context.Context, string) (upstreamv1.ResolvedTarget, error) {
	return upstreamv1.ResolvedTarget{ID: "u/m", Protocol: "anthropic", NativeModel: "native", BaseURL: a.provider}, nil
}
func (a authUpstream) Report(context.Context, upstreamv1.ResultReport) error { return nil }

func TestProductionUserModelKeyCannotBeBypassedByGlobalKey(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","model":"native","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer provider.Close()
	store, err := relaystore.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	_ = store.PutUserModel(ctx, relaystore.UserModel{Name: "m", Protocol: "anthropic", APIKey: "model-key", Enabled: true})
	_ = store.PutGroup(ctx, relaystore.Group{ID: "g", UserModel: "m", PolicyType: "sticky", PolicyConfig: "{}"})
	_ = store.AddMembers(ctx, "g", []string{"u/m"})
	s := httptest.NewServer(NewRelay(&config.Config{APIKey: "global-key"}, &schedule.Scheduler{Store: store, Upstream: authUpstream{provider: provider.URL}}).Handler())
	defer s.Close()
	body := []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	request := func(key string) *http.Response {
		r, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/messages", bytes.NewReader(body))
		r.Header.Set("x-api-key", key)
		r.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := request("global-key")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("global bypass status=%d", resp.StatusCode)
	}
	resp = request("model-key")
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil || resp.StatusCode != 200 {
		t.Fatalf("model key status=%d decoded=%v err=%v", resp.StatusCode, decoded, err)
	}
}
