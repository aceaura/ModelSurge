package upstreamstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/dialect"
)

// createModeOneSource 构造一个"模式一形态"的 SQLite upstream.db：
// 账号 + 运行态（model_state/quota_state/result_reports/usage_log）。
// suffix 非空时拼进账号/报表名（PG gated 用例对同一库重复执行不撞行）。
func createModeOneSource(t *testing.T, suffix ...string) string {
	t.Helper()
	sfx := ""
	if len(suffix) > 0 {
		sfx = suffix[0]
	}
	acct := "acct" + sfx
	kacct := "kacct" + sfx
	path := filepath.Join(t.TempDir(), "source-upstream.db")
	src, err := Open(dialect.SQLite, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Accounts.InsertAccount(&account.Account{
		Name: acct, Type: account.TypeAPIKey, Enabled: true,
		Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "sk",
		Models: map[string]string{"m1": "n1", "m2": "n2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := src.Accounts.InsertAccount(&account.Account{
		Name: kacct, Type: account.TypeKiro, Enabled: true,
		Kiro: &account.KiroAccount{Source: account.SourceRefreshToken, RefreshToken: "rt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := src.Accounts.SaveTokenState(kacct, &account.TokenState{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := src.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	// m1 打上失败/冷却态；m2 配额态
	if _, err := src.ApplyReport(context.Background(), upstreamv1.ResultReport{ReportID: "rep1" + sfx, TargetID: acct + "/m1", Outcome: "auth_error", Status: 401}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.DB.Exec(`INSERT INTO quota_state(upstream_model_id,limit_kind,remaining,raw,checked_at) VALUES(?, '7h',3.5,'{"x":1}',123)`, acct+"/m2"); err != nil {
		t.Fatal(err)
	}
	if err := src.Accounts.InsertUsage(acct, time.Unix(2000, 0), account.Usage{InputTokens: 7}); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateFreshIdempotentAndReadOnly(t *testing.T) {
	ctx := context.Background()
	source := createModeOneSource(t)
	dst, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	res, err := dst.Migrate(ctx, source)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.Accounts != 2 || res.States != 3 || res.Reports != 1 || res.Usage != 1 {
		t.Fatalf("migrate result = %+v", res)
	}

	// 数据核对：模型行物化齐 + model_state 覆盖 + quota + 账本 + usage
	models, err := dst.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// acct（2 模型）+ kacct（1 透传模型 *）
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	var failures int
	var cooldown int64
	if err := dst.DB.QueryRow(`SELECT failures,cooldown_until FROM model_state WHERE upstream_model_id='acct/m1'`).Scan(&failures, &cooldown); err != nil {
		t.Fatal(err)
	}
	if failures != 1 || cooldown <= 0 {
		t.Fatalf("m1 state: failures=%d cooldown=%d", failures, cooldown)
	}
	var remaining float64
	var raw string
	if err := dst.DB.QueryRow(`SELECT remaining,raw FROM quota_state WHERE upstream_model_id='acct/m2'`).Scan(&remaining, &raw); err != nil {
		t.Fatal(err)
	}
	if remaining != 3.5 || raw != `{"x":1}` {
		t.Fatalf("m2 quota: %v %q", remaining, raw)
	}
	ts, err := dst.Accounts.GetTokenState("kacct")
	if err != nil || ts == nil || ts.AccessToken != "at" {
		t.Fatalf("kacct token state: %+v %v", ts, err)
	}
	var usage int
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 1 {
		t.Fatalf("usage = %d", usage)
	}

	// 重跑：整体短路（marker），usage 不重复
	res2, err := dst.Migrate(ctx, source)
	if err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if res2.Accounts != 0 || res2.Usage != 0 {
		t.Fatalf("re-migrate duplicated: %+v", res2)
	}
	if err := dst.DB.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 1 {
		t.Fatalf("usage after re-run = %d", usage)
	}

	// 源库只读：两轮迁移后内容不变（usage 仍 1 行）
	ro, err := dialect.OpenSQLiteReadOnly(mustAbs(t, source))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var srcUsage int
	if err := ro.QueryRow(`SELECT COUNT(*) FROM usage_log`).Scan(&srcUsage); err != nil {
		t.Fatal(err)
	}
	if srcUsage != 1 {
		t.Fatalf("source mutated: usage=%d", srcUsage)
	}
}

// 迁移目标与源同文件：拒绝。
func TestMigrateRejectsSameFile(t *testing.T) {
	source := createModeOneSource(t)
	s, err := Open(dialect.SQLite, source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Migrate(context.Background(), source); err == nil {
		t.Fatal("migrating into the same file should fail")
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
