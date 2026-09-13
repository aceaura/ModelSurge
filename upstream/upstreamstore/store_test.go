package upstreamstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"

	_ "modernc.org/sqlite"
)

func TestMaterializeAndIdempotentReport(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Accounts.InsertAccount(&account.Account{Name: "a", Type: account.TypeAPIKey, Enabled: true, Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "secret", Models: map[string]string{"model": "native"}}); err != nil {
		t.Fatal(err)
	}
	if err = s.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	models, err := s.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "a/model" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	r := upstreamv1.ResultReport{ReportID: "same", TargetID: "a/model", Outcome: "normal"}
	a, err := s.ApplyReport(context.Background(), r)
	if err != nil || !a {
		t.Fatalf("first=%v err=%v", a, err)
	}
	a, err = s.ApplyReport(context.Background(), r)
	if err != nil || a {
		t.Fatalf("second=%v err=%v", a, err)
	}
}

func createLegacy(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := account.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAccount(&account.Account{Name: "legacy", Type: account.TypeAPIKey, Enabled: true, Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "secret", Models: map[string]string{"public": "native"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertUsage("legacy", time.Unix(1234, 0), account.Usage{InputTokens: 10, OutputTokens: 4, CacheRead: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportLegacyFreshRepeatAndUsage(t *testing.T) {
	ctx := context.Background()
	legacy := createLegacy(t)
	dst, err := Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.ImportLegacy(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	models, err := dst.ListModels(ctx)
	if err != nil || len(models) != 1 || models[0].ID != "legacy/public" || models[0].NativeModel != "native" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	var accounts, usage, imports int
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM legacy_imports`).Scan(&imports); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 || usage != 1 || imports != 1 {
		t.Fatalf("accounts=%d usage=%d imports=%d", accounts, usage, imports)
	}
	if err := dst.ImportLegacy(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 1 {
		t.Fatalf("repeat import duplicated usage: %d", usage)
	}
	ro, err := sql.Open("sqlite", "file:"+filepath.ToSlash(legacy)+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var sourceAccounts, sourceUsage int
	_ = ro.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&sourceAccounts)
	_ = ro.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&sourceUsage)
	if sourceAccounts != 1 || sourceUsage != 1 {
		t.Fatalf("legacy source changed: accounts=%d usage=%d", sourceAccounts, sourceUsage)
	}
}

func TestImportLegacyFailureRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	legacy := createLegacy(t)
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE accounts SET models='not-json' WHERE name='legacy'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	dst, err := Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.ImportLegacy(ctx, legacy); err == nil {
		t.Fatal("expected invalid legacy data to fail")
	}
	for _, table := range []string{"accounts", "upstream_models", "usage_log", "legacy_usage_imports", "legacy_imports"} {
		var n int
		if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s partially committed %d rows", table, n)
		}
	}
}
