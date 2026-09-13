package upstreamclient

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"relayd/backend/account"
	"relayd/backend/contract/upstreamv1"
	"relayd/backend/upstream"
	"relayd/backend/upstreamstore"
)

func TestInternalAPIAuthenticatedResolveEvaluateReport(t *testing.T) {
	store, err := upstreamstore.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Accounts.InsertAccount(&account.Account{Name: "a", Type: account.TypeAPIKey, Enabled: true, Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "secret", Models: map[string]string{"m": "native"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(upstream.NewHTTPServer(upstream.NewService(store, mgr), "service-key").Handler())
	defer ts.Close()
	bad := New(ts.URL, "wrong", 0)
	if err := bad.Health(context.Background()); err == nil {
		t.Fatal("wrong service key unexpectedly authenticated")
	}
	cl := New(ts.URL, "service-key", 0)
	models, err := cl.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "a/m" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	target, err := cl.Resolve(context.Background(), "a/m")
	if err != nil || target.APIKey != "secret" || target.NativeModel != "native" {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	evals, err := cl.Evaluate(context.Background(), []string{"a/m"})
	if err != nil || len(evals) != 1 || !evals[0].Available {
		t.Fatalf("evals=%+v err=%v", evals, err)
	}
	report := upstreamv1.ResultReport{ReportID: "same", TargetID: "a/m", Outcome: "normal"}
	if err := cl.Report(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if err := cl.Report(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	var reports int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM result_reports`).Scan(&reports); err != nil || reports != 1 {
		t.Fatalf("reports=%d err=%v", reports, err)
	}
}
