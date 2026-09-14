package upstreamstore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/dialect/pgtest"
)

// openPG 打开模块专属 PG 库（三库三 role 对应的分库；TEST_PG_DSN 未设时跳过）。
func openPG(t *testing.T) *Store {
	t.Helper()
	s, err := Open(dialect.Postgres, pgtest.ModuleDSN(t, "upstream"))
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// pgUnique 每次调用唯一短后缀：gated 测试对同一 PG 库重复执行不撞行。
var pgUnique = time.Now().UnixNano()

// postgres 门控：并发 ApplyReport 失败计数不丢更新（FOR UPDATE + 原子自增）。
func TestApplyReport_ConcurrentPG(t *testing.T) {
	s := openPG(t)
	ctx := context.Background()
	acct := fmt.Sprintf("pg-acct-%d", pgUnique)
	if err := s.Accounts.InsertAccount(&account.Account{
		Name: acct, Type: account.TypeAPIKey, Enabled: true,
		Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "k",
		Models: map[string]string{"m": "n"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	target := acct + "/m"

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.ApplyReport(ctx, upstreamv1.ResultReport{
				ReportID: reportID(acct, i), TargetID: target,
				Outcome: "auth_error", Status: 401,
			}); err != nil {
				t.Errorf("apply %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	var failures int
	if err := s.DB.QueryRow(s.q(`SELECT failures FROM model_state WHERE upstream_model_id=?`), target).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != n {
		t.Fatalf("concurrent failures = %d, want %d (lost updates)", failures, n)
	}
}

// postgres 门控：物化保运行态（同 sqlite 回归语义在 PG 复核）。
func TestMaterializeKeepsModelStatePG(t *testing.T) {
	s := openPG(t)
	ctx := context.Background()
	acct := fmt.Sprintf("pg-mat-%d", pgUnique)
	if err := s.Accounts.InsertAccount(&account.Account{
		Name: acct, Type: account.TypeAPIKey, Enabled: true,
		Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "k",
		Models: map[string]string{"m1": "n1", "m2": "n2"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReport(ctx, upstreamv1.ResultReport{ReportID: reportID(acct, 0), TargetID: acct + "/m1", Outcome: "auth_error", Status: 401}); err != nil {
		t.Fatal(err)
	}
	if err := s.Accounts.UpdateAccount(&account.Account{
		Name: acct, Type: account.TypeAPIKey, Enabled: true,
		Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "k",
		Models: map[string]string{"m1": "n1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MaterializeAccounts(); err != nil {
		t.Fatal(err)
	}
	var failures int
	if err := s.DB.QueryRow(s.q(`SELECT failures FROM model_state WHERE upstream_model_id=?`), acct+"/m1").Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Fatalf("re-materialize reset state on pg: failures=%d", failures)
	}
	var stale int
	if err := s.DB.QueryRow(s.q(`SELECT COUNT(*) FROM model_state WHERE upstream_model_id=?`), acct+"/m2").Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("removed model state survived on pg: %d", stale)
	}
}

// postgres 门控：SQLite→PG 迁移全链路（迁移工具的目标形态）。
func TestMigrateSQLiteToPG(t *testing.T) {
	s := openPG(t)
	ctx := context.Background()
	sfx := fmt.Sprintf("-%d", pgUnique)
	acct := "acct" + sfx
	source := createModeOneSource(t, sfx)
	res, err := s.Migrate(ctx, source)
	if err != nil {
		t.Fatalf("migrate to pg: %v", err)
	}
	if res.Accounts != 2 || res.States != 3 || res.Reports != 1 || res.Usage != 1 {
		t.Fatalf("migrate result = %+v", res)
	}
	// 状态核对
	var failures int
	if err := s.DB.QueryRow(s.q(`SELECT failures FROM model_state WHERE upstream_model_id=?`), acct+"/m1").Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Fatalf("m1 failures = %d", failures)
	}
	var remaining *float64
	if err := s.DB.QueryRow(s.q(`SELECT remaining FROM quota_state WHERE upstream_model_id=?`), acct+"/m2").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining == nil || *remaining != 3.5 {
		t.Fatalf("m2 quota remaining = %v", remaining)
	}
	// 重跑短路
	res2, err := s.Migrate(ctx, source)
	if err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if res2.Usage != 0 {
		t.Fatalf("re-migrate duplicated usage: %+v", res2)
	}
}

func reportID(acct string, i int) string {
	return fmt.Sprintf("%s-report-%d", acct, i)
}
