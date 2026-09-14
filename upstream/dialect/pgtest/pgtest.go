// Package pgtest 供 PG 门控测试按模块分库：三模块 schema 存在同名表冲突
// （upstream 与 replay 的 result_reports 列不同，CREATE TABLE IF NOT EXISTS
// 不改已存在表），共库必炸——与部署形态的「三库三 role」约束一致。
// 只应被 _test.go 引用，不进生产二进制。
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/aceaura/ModelSurge/upstream/dialect"
)

// ModuleDSN 返回模块专属库的 DSN：TEST_PG_DSN 未设时跳过测试；
// 在其基础上派生 <base>_<module> 库，不存在则经 admin 连接创建。
// module 取值：upstream（account 与 upstreamstore 共用，同 schema 域）、
// replay、agent。
func ModuleDSN(t *testing.T, module string) string {
	t.Helper()
	base := os.Getenv("TEST_PG_DSN")
	if base == "" {
		t.Skip("TEST_PG_DSN not set")
	}
	name := dialect.DatabaseName(base) + "_" + module
	dsn, err := dialect.WithDatabase(base, name)
	if err != nil {
		t.Fatalf("pgtest: derive %s dsn: %v", module, err)
	}
	ensureDatabase(t, base, name)
	return dsn
}

func ensureDatabase(t *testing.T, adminDSN, name string) {
	t.Helper()
	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("pgtest: open admin: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if exists(t, db, ctx, name) {
		return
	}
	// CREATE DATABASE 不能在事务里执行
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		// 并发创建竞态：他人先建成即为成功
		if !exists(t, db, ctx, name) {
			t.Fatalf("pgtest: create database %s: %v", name, err)
		}
	}
}

func exists(t *testing.T, db *sql.DB, ctx context.Context, name string) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&ok); err != nil {
		t.Fatalf("pgtest: check database %s: %v", name, err)
	}
	return ok
}
