package upstreamstore

import (
	"context"
	"database/sql"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"

	_ "modernc.org/sqlite"
)

// 再物化保运行态：存续模型的熔断/冷却计数不被重置（启动/热更语义），
// 配置中移除的模型行（连同其 state）被清掉。
func TestMaterializeKeepsModelState(t *testing.T) {
	ctx := context.Background()
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Accounts.InsertAccount(&account.Account{Name: "a", Type: account.TypeAPIKey, Enabled: true, Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "secret", Models: map[string]string{"m1": "n1", "m2": "n2"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReport(ctx, upstreamv1.ResultReport{ReportID: "r1", TargetID: "a/m1", Outcome: "auth_error", Status: 401}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReport(ctx, upstreamv1.ResultReport{ReportID: "r2", TargetID: "a/m1", Outcome: "auth_error", Status: 401}); err != nil {
		t.Fatal(err)
	}

	// 账号配置收缩为只剩 m1：m2 行应被清，m1 计数应保
	if err := s.Accounts.UpdateAccount(&account.Account{Name: "a", Type: account.TypeAPIKey, Enabled: true, Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "secret", Models: map[string]string{"m1": "n1"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	var failures int
	if err := s.DB.QueryRow(`SELECT failures FROM model_state WHERE upstream_model_id='a/m1'`).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 2 {
		t.Fatalf("re-materialize reset model_state: failures=%d, want 2", failures)
	}
	var cooldown int64
	if err := s.DB.QueryRow(`SELECT cooldown_until FROM model_state WHERE upstream_model_id='a/m1'`).Scan(&cooldown); err != nil {
		t.Fatal(err)
	}
	if cooldown <= 0 {
		t.Fatalf("re-materialize reset cooldown: %d", cooldown)
	}
	var staleRows int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM model_state WHERE upstream_model_id='a/m2'`).Scan(&staleRows); err != nil {
		t.Fatal(err)
	}
	if staleRows != 0 {
		t.Fatalf("removed model's state survived: %d", staleRows)
	}
}

func TestMaterializeAndIdempotentReport(t *testing.T) {
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "upstream.db"))
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
	st, err := account.Open(dialect.SQLite, path)
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

func TestMaterializeGeminiAccountKeepsAccountAndRemovesModels(t *testing.T) {
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Accounts.InsertAccount(&account.Account{Name: "legacy-gemini", Type: account.TypeAPIKey, Enabled: true, Protocol: "gemini", BaseURL: "https://example.test", APIKey: "secret", Models: map[string]string{"public": "gemini-pro"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO upstream_models(id,account,display_name,protocol,native_model,base_url,headers,request_overrides,enabled,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"legacy-gemini/public", "legacy-gemini", "public", "gemini", "gemini-pro", "https://example.test", "{}", "null", 1, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := s.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	var accounts, models int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM accounts WHERE name='legacy-gemini'`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM upstream_models WHERE account='legacy-gemini'`).Scan(&models); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 || models != 0 {
		t.Fatalf("accounts=%d models=%d, want account preserved and models removed", accounts, models)
	}
}

func TestImportLegacyFreshRepeatAndUsage(t *testing.T) {
	ctx := context.Background()
	legacy := createLegacy(t)
	dst, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "upstream.db"))
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

func TestImportLegacyGeminiAccountDoesNotPublishModels(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "legacy-gemini.db")
	legacy, err := account.Open(dialect.SQLite, legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.InsertAccount(&account.Account{Name: "legacy-gemini", Type: account.TypeAPIKey, Enabled: true, Protocol: "gemini", BaseURL: "https://example.test", APIKey: "secret", Models: map[string]string{"public": "gemini-pro"}}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	dst, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.ImportLegacy(context.Background(), legacyPath); err != nil {
		t.Fatal(err)
	}
	var accounts, models int
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM accounts WHERE name='legacy-gemini'`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM upstream_models WHERE account='legacy-gemini'`).Scan(&models); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 || models != 0 {
		t.Fatalf("accounts=%d models=%d, want imported account without published models", accounts, models)
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
	dst, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "upstream.db"))
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
