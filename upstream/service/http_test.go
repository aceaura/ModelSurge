package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/adminapi"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

func TestInternalAuthRejectsMissingKey(t *testing.T) {
	s := NewHTTPServer(nil, "secret")
	r := httptest.NewRequest(http.MethodGet, "/internal/v1/health", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", w.Code)
	}
	if body := w.Body.String(); body == "" || body == "secret" {
		t.Fatalf("unsafe body %q", body)
	}
}

func TestAccessLogDisabledSilencesUpstreamHTTP(t *testing.T) {
	h := NewHTTPServer(nil, "service-key")
	h.AccessLogEnabled = false
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	r := httptest.NewRequest(http.MethodGet, upstreamv1.BasePath+"/health", nil)
	r.Header.Set("Authorization", "Bearer service-key")
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if logs.Len() != 0 {
		t.Fatalf("access_log=false emitted logs: %s", logs.String())
	}
}

func TestAccountAdminBelongsToUpstream(t *testing.T) {
	store, err := upstreamstore.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Accounts.InsertAccount(&account.Account{Name: "a", Type: account.TypeAPIKey, Enabled: true, Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "secret"}); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(NewService(store, mgr), "service-key")
	h.AdminHandler = adminapi.NewAccountAdmin(mgr, "admin-key")
	r := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("missing admin key status=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	r.Header.Set("X-Admin-Key", "admin-key")
	w = httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() == "" || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAdminRejectsGeminiProtocol(t *testing.T) {
	store, err := upstreamstore.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	h := adminapi.NewAccountAdmin(mgr, "admin-key")
	r := httptest.NewRequest(http.MethodPost, "/admin/accounts", strings.NewReader(`{"name":"gemini","type":"api-key","protocol":"gemini","base_url":"https://example.test","api_key":"secret","models":{"public":"gemini-pro"}}`))
	r.Header.Set("X-Admin-Key", "admin-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "protocol") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestModelsFilterAndResolveRejectsStaleGeminiRow(t *testing.T) {
	store, err := upstreamstore.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Accounts.InsertAccount(&account.Account{Name: "gemini", Type: account.TypeAPIKey, Enabled: true, Protocol: "gemini", BaseURL: "https://example.test", APIKey: "secret", Models: map[string]string{"public": "gemini-pro"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.Exec(`INSERT INTO upstream_models(id,account,display_name,protocol,native_model,base_url,headers,request_overrides,enabled,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"gemini/public", "gemini", "public", "gemini", "gemini-pro", "https://example.test", "{}", "null", 1, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, mgr)
	models, err := svc.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		b, _ := json.Marshal(models)
		t.Fatalf("Gemini model published: %s", b)
	}
	if target, rerr := svc.Resolve(context.Background(), "gemini/public"); rerr == nil || target.ID != "" || rerr.Code != upstreamv1.CodeTargetUnavailable {
		t.Fatalf("target=%+v err=%+v", target, rerr)
	}
}

func TestResolveKiroReturnsOpaqueTarget(t *testing.T) {
	svc, _ := testKiroService(t)
	target, err := svc.Resolve(context.Background(), "kiro/*")
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "kiro/*" || target.Protocol != "kiro" {
		t.Fatalf("target identity = %+v", target)
	}
	if target.NativeModel != "" || target.Account != "" || target.BaseURL != "" || target.APIKey != "" || target.Headers != nil || target.RequestOverrides != nil || target.Runtime != (upstreamv1.RuntimeMetadata{}) {
		t.Fatalf("Kiro target exposed runtime details: %+v", target)
	}
}

func TestKiroInvalidModelSwitchesWithoutPenalty(t *testing.T) {
	svc, store := testKiroService(t)
	result, err := svc.Report(context.Background(), upstreamv1.ResultReport{
		ReportID: "invalid-model", TargetID: "kiro/*", Outcome: "abnormal", Status: 400,
		Reason: account.ReasonInvalidModelID,
	})
	if err != nil || result.Action != upstreamv1.ActionSwitchTarget {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	model, err := store.GetModel(context.Background(), "kiro/*")
	if err != nil || model.Failures != 0 || !model.CooldownUntil.IsZero() {
		t.Fatalf("model=%+v err=%v", model, err)
	}
}

func TestTransientRetriesThenOpensBreaker(t *testing.T) {
	svc, store := testKiroService(t)
	first, err := svc.Report(context.Background(), upstreamv1.ResultReport{
		ReportID: "transient-1", TargetID: "kiro/*", Outcome: "abnormal", Status: 502, Attempt: 0,
	})
	if err != nil || first.Action != upstreamv1.ActionRetryTarget {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	model, _ := store.GetModel(context.Background(), "kiro/*")
	if model.Failures != 0 || !model.CooldownUntil.IsZero() {
		t.Fatalf("retry attempt penalized model: %+v", model)
	}
	second, err := svc.Report(context.Background(), upstreamv1.ResultReport{
		ReportID: "transient-2", TargetID: "kiro/*", Outcome: "abnormal", Status: 502, Attempt: 1,
	})
	if err != nil || second.Action != upstreamv1.ActionSwitchTarget {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	model, _ = store.GetModel(context.Background(), "kiro/*")
	if model.Failures != 1 || model.CooldownUntil.IsZero() {
		t.Fatalf("breaker not opened: %+v", model)
	}
}

func testKiroService(t *testing.T) (*Service, *upstreamstore.Store) {
	t.Helper()
	store, err := upstreamstore.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Accounts.InsertAccount(&account.Account{
		Name: "kiro", Type: account.TypeKiro, Enabled: true, Models: map[string]string{"*": "*"},
		Kiro: &account.KiroAccount{RefreshToken: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(store, mgr), store
}
