// migrate.go 模式一→模式二数据迁移：SQLite upstream.db 只读源 → 当前库
// （design/deployment-modes.md 2.2）。整体一次性（legacy_imports 源路径
// 标记）、行级幂等（唯一约束 upsert），重跑不重复不丢失。
package upstreamstore

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/dialect"
)

// MigrateResult 各表新迁入行数（已存在行不计）。
type MigrateResult struct {
	Accounts    int64
	States      int64
	QuotaStates int64
	Reports     int64
	Usage       int64
}

// Migrate 从模式一 SQLite upstream.db 只读迁入当前库（幂等，可重跑）。
// 账号（身份+凭据+调度态）→ 物化模型行 → model_state/quota_state 覆盖 →
// result_reports 账本 → usage_log（rowid 标记防重）。target_cache 类
// 派生态不迁（MaterializeAccounts 重建）。
func (s *Store) Migrate(ctx context.Context, source string) (*MigrateResult, error) {
	abs, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	if s.path != "" {
		if same, _ := filepath.Abs(s.path); same == abs {
			return nil, fmt.Errorf("migrate source must differ from target db")
		}
	}
	var done int
	if err := s.DB.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM legacy_imports WHERE source_path=?`), abs).Scan(&done); err != nil {
		return nil, err
	}
	if done > 0 {
		log.Printf("upstreamstore: migrate %s already imported, skipping", abs)
		return &MigrateResult{}, nil
	}
	src, err := dialect.OpenSQLiteReadOnly(abs)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res := &MigrateResult{}

	// 1. accounts 原样列级拷贝（含 token_state/冷却/熔断/统计）；
	// 返回解析出的账号（同事务内物化——pool 侧读不到未提交行）
	accs, n, err := s.copyAccounts(ctx, src, tx)
	res.Accounts = n
	if err != nil {
		return nil, err
	}

	// 2. 模型行经物化重建（物化保运行态：model_state 行须先有父行，
	// 故物化在前、状态覆盖在后）
	for _, a := range accs {
		if err := materializeAccount(s, tx, a); err != nil {
			return nil, err
		}
	}

	// 3. model_state（源缺表则跳过）
	has, err := sqliteHasTable(src, "model_state")
	if err != nil {
		return nil, err
	}
	if has {
		n, err = s.copyModelState(ctx, src, tx)
		res.States = n
		if err != nil {
			return nil, err
		}
	}

	// 4. quota_state（源缺表则跳过；remaining 可空）
	has, err = sqliteHasTable(src, "quota_state")
	if err != nil {
		return nil, err
	}
	if has {
		n, err = s.copyQuotaState(ctx, src, tx)
		res.QuotaStates = n
		if err != nil {
			return nil, err
		}
	}

	// 5. result_reports 幂等账本（源缺表则跳过）
	has, err = sqliteHasTable(src, "result_reports")
	if err != nil {
		return nil, err
	}
	if has {
		n, err = s.copyResultReports(ctx, src, tx)
		res.Reports = n
		if err != nil {
			return nil, err
		}
	}

	// 6. usage_log（rowid 标记防重）
	n, err = s.copyUsage(ctx, src, tx, abs)
	res.Usage = n
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO legacy_imports(source_path,imported_at) VALUES(?,?)`), abs, time.Now().Unix()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	log.Printf("upstreamstore: migrated %s: accounts=%d states=%d quota=%d reports=%d usage=%d",
		abs, res.Accounts, res.States, res.QuotaStates, res.Reports, res.Usage)
	return res, nil
}

// copyAccounts 逐行读源 accounts 并按 name 幂等 upsert，返回解析出的
// 账号（供同事务内物化）与新迁入行数。
func (s *Store) copyAccounts(ctx context.Context, src *sql.DB, tx *sql.Tx) ([]account.Account, int64, error) {
	rows, err := src.QueryContext(ctx, `SELECT name,type,enabled,protocol,base_url,api_key,models,headers,models_allowlist,kiro,token_state,overrides,disabled,limit_kind,cooldown_until,failures,last_failure,stats,updated_at FROM accounts ORDER BY rowid`)
	if err != nil {
		return nil, 0, fmt.Errorf("read source accounts: %w", err)
	}
	var copied int64
	var accs []account.Account
	for rows.Next() {
		vals := make([]any, 19)
		ptrs := make([]any, 19)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return nil, 0, err
		}
		a, err := legacyAccount(vals)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		accs = append(accs, a)
		res, err := tx.ExecContext(ctx, s.q(`INSERT INTO accounts(name,type,enabled,protocol,base_url,api_key,models,headers,models_allowlist,kiro,token_state,overrides,disabled,limit_kind,cooldown_until,failures,last_failure,stats,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET type=excluded.type,enabled=excluded.enabled,protocol=excluded.protocol,base_url=excluded.base_url,api_key=excluded.api_key,models=excluded.models,headers=excluded.headers,models_allowlist=excluded.models_allowlist,kiro=excluded.kiro,token_state=excluded.token_state,overrides=excluded.overrides,disabled=excluded.disabled,limit_kind=excluded.limit_kind,cooldown_until=excluded.cooldown_until,failures=excluded.failures,last_failure=excluded.last_failure,stats=excluded.stats,updated_at=excluded.updated_at`),
			vals...)
		if err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("import account %s: %w", a.Name, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			copied += n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	rows.Close()
	return accs, copied, nil
}

func (s *Store) copyModelState(ctx context.Context, src *sql.DB, tx *sql.Tx) (int64, error) {
	rows, err := src.QueryContext(ctx, `SELECT upstream_model_id,cooldown_until,failures,last_error_class,updated_at FROM model_state ORDER BY upstream_model_id`)
	if err != nil {
		return 0, fmt.Errorf("read source model_state: %w", err)
	}
	var copied int64
	for rows.Next() {
		var id, class string
		var cooldown, updatedAt int64
		var failures int
		if err := rows.Scan(&id, &cooldown, &failures, &class, &updatedAt); err != nil {
			rows.Close()
			return 0, err
		}
		if strings.TrimSpace(id) == "" {
			rows.Close()
			return 0, fmt.Errorf("model_state row with empty upstream_model_id")
		}
		res, err := tx.ExecContext(ctx, s.q(`INSERT INTO model_state(upstream_model_id,cooldown_until,failures,last_error_class,updated_at) VALUES(?,?,?,?,?)
			ON CONFLICT(upstream_model_id) DO UPDATE SET cooldown_until=excluded.cooldown_until,failures=excluded.failures,last_error_class=excluded.last_error_class,updated_at=excluded.updated_at`),
			id, cooldown, failures, class, updatedAt)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("import model_state %s: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			copied += n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	return copied, nil
}

func (s *Store) copyQuotaState(ctx context.Context, src *sql.DB, tx *sql.Tx) (int64, error) {
	rows, err := src.QueryContext(ctx, `SELECT upstream_model_id,limit_kind,remaining,raw,checked_at FROM quota_state ORDER BY upstream_model_id`)
	if err != nil {
		return 0, fmt.Errorf("read source quota_state: %w", err)
	}
	var copied int64
	for rows.Next() {
		var id, kind, raw string
		var remaining sql.NullFloat64
		var checkedAt int64
		if err := rows.Scan(&id, &kind, &remaining, &raw, &checkedAt); err != nil {
			rows.Close()
			return 0, err
		}
		if strings.TrimSpace(id) == "" {
			rows.Close()
			return 0, fmt.Errorf("quota_state row with empty upstream_model_id")
		}
		res, err := tx.ExecContext(ctx, s.q(`INSERT INTO quota_state(upstream_model_id,limit_kind,remaining,raw,checked_at) VALUES(?,?,?,?,?)
			ON CONFLICT(upstream_model_id) DO UPDATE SET limit_kind=excluded.limit_kind,remaining=excluded.remaining,raw=excluded.raw,checked_at=excluded.checked_at`),
			id, kind, remaining, raw, checkedAt)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("import quota_state %s: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			copied += n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	return copied, nil
}

func (s *Store) copyResultReports(ctx context.Context, src *sql.DB, tx *sql.Tx) (int64, error) {
	rows, err := src.QueryContext(ctx, `SELECT report_id,upstream_model_id,received_at FROM result_reports ORDER BY rowid`)
	if err != nil {
		return 0, fmt.Errorf("read source result_reports: %w", err)
	}
	var copied int64
	for rows.Next() {
		var reportID, targetID string
		var receivedAt int64
		if err := rows.Scan(&reportID, &targetID, &receivedAt); err != nil {
			rows.Close()
			return 0, err
		}
		if strings.TrimSpace(reportID) == "" {
			rows.Close()
			return 0, fmt.Errorf("result report with empty report_id")
		}
		res, err := tx.ExecContext(ctx, s.q(`INSERT INTO result_reports(report_id,upstream_model_id,received_at) VALUES(?,?,?) ON CONFLICT(report_id) DO NOTHING`),
			reportID, targetID, receivedAt)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("import result report %s: %w", reportID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			copied += n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	return copied, nil
}

// copyUsage 与 ImportLegacy 同一防重模式：rowid 标记表 + RETURNING id。
func (s *Store) copyUsage(ctx context.Context, src *sql.DB, tx *sql.Tx, abs string) (int64, error) {
	rows, err := src.QueryContext(ctx, `SELECT rowid,account,ts,input_tokens,output_tokens,cache_read,cache_creation FROM usage_log ORDER BY rowid`)
	if err != nil {
		return 0, fmt.Errorf("read source usage: %w", err)
	}
	var copied int64
	for rows.Next() {
		var rowID, ts, input, output, cacheRead, cacheCreation int64
		var accountName string
		if err := rows.Scan(&rowID, &accountName, &ts, &input, &output, &cacheRead, &cacheCreation); err != nil {
			rows.Close()
			return 0, err
		}
		if strings.TrimSpace(accountName) == "" {
			rows.Close()
			return 0, fmt.Errorf("usage row %d has empty account", rowID)
		}
		var usageID int64
		if err := tx.QueryRowContext(ctx, s.q(`INSERT INTO usage_log(account,ts,input_tokens,output_tokens,cache_read,cache_creation) VALUES(?,?,?,?,?,?) RETURNING id`),
			accountName, ts, input, output, cacheRead, cacheCreation).Scan(&usageID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("import usage row %d: %w", rowID, err)
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO legacy_usage_imports(source_path,source_rowid,usage_id) VALUES(?,?,?)`), abs, rowID, usageID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("record usage row %d: %w", rowID, err)
		}
		copied++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	return copied, nil
}

// sqliteHasTable 源库（只读 sqlite）是否有某表。
func sqliteHasTable(src *sql.DB, table string) (bool, error) {
	var n int
	if err := src.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
