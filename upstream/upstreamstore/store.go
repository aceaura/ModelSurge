// Package upstreamstore owns upstream.db, provider accounts, model catalog and runtime state.
package upstreamstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/ir"
)

type Store struct {
	DB       *sql.DB
	Accounts *account.Store
	driver   string
	// path sqlite 模式下的库路径（ImportLegacy 源路径防混用）；postgres 为空。
	path string
}

type Model struct {
	ID               string
	Account          string
	DisplayName      string
	Protocol         string
	NativeModel      string
	BaseURL          string
	Headers          map[string]string
	RequestOverrides json.RawMessage
	Enabled          bool
	// ContextWindow 上下文窗口 token 数；0=未知（调度不过滤）。
	ContextWindow int
	CooldownUntil time.Time
	Failures      int
	// LastErrorClass 最近一次错误的归类（auth/rate_limit/cooldown_until/
	// outcome）：Half-Open 试探只对熔断退避类放行，配额/限流/鉴权类严格跳过。
	LastErrorClass string
}

// schema 时间戳列用 BIGINT（postgres 8 字节；sqlite 亲和性与 INTEGER 等价）。
const schema = `
CREATE TABLE IF NOT EXISTS upstream_models (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, display_name TEXT NOT NULL,
 protocol TEXT NOT NULL, native_model TEXT NOT NULL, base_url TEXT NOT NULL,
 headers TEXT NOT NULL DEFAULT '{}', request_overrides TEXT NOT NULL DEFAULT '{}',
 enabled INTEGER NOT NULL DEFAULT 1, context_window INTEGER NOT NULL DEFAULT 0, updated_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS model_state (
 upstream_model_id TEXT PRIMARY KEY REFERENCES upstream_models(id) ON DELETE CASCADE,
 cooldown_until BIGINT NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0,
 last_error_class TEXT NOT NULL DEFAULT '', updated_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS quota_state (
 upstream_model_id TEXT PRIMARY KEY REFERENCES upstream_models(id) ON DELETE CASCADE,
 limit_kind TEXT NOT NULL DEFAULT '', remaining DOUBLE PRECISION, raw TEXT NOT NULL DEFAULT '{}', checked_at BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS result_reports (
 report_id TEXT PRIMARY KEY, upstream_model_id TEXT NOT NULL, received_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS legacy_imports (
 source_path TEXT PRIMARY KEY, imported_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS legacy_usage_imports (
 source_path TEXT NOT NULL, source_rowid BIGINT NOT NULL, usage_id BIGINT NOT NULL,
 PRIMARY KEY(source_path,source_rowid), UNIQUE(usage_id)
);
`

func Open(driver, dsn string) (*Store, error) {
	if !dialect.Valid(driver) {
		return nil, fmt.Errorf("upstreamstore: unsupported driver %q", driver)
	}
	db, err := dialect.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	accounts, err := account.OpenDB(db, driver)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("upstreamstore: migrate: %w", err)
	}
	// CREATE TABLE IF NOT EXISTS 不改已存在表：存量库补 context_window 列。
	if _, err := db.Exec(`ALTER TABLE upstream_models ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !columnExists(err) {
			db.Close()
			return nil, fmt.Errorf("upstreamstore: add context_window: %w", err)
		}
	}
	path := dialect.SQLitePath(driver, dsn)
	return &Store{DB: db, Accounts: accounts, driver: driver, path: path}, nil
}

// q 按方言重写占位符（postgres ? → $n）。
func (s *Store) q(query string) string { return dialect.Rebind(s.driver, query) }

// columnExists 判定 ALTER ADD COLUMN 的「列已存在」错误（sqlite/pg 方言串不同）。
func columnExists(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column name") || strings.Contains(msg, "already exists")
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) MaterializeAccounts() error {
	accs, err := s.Accounts.ListAccounts()
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(s.q(`DELETE FROM upstream_models WHERE account NOT IN (SELECT name FROM accounts)`)); err != nil {
		return err
	}
	for _, a := range accs {
		if err := materializeAccount(s, tx, a); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func materializeAccount(s *Store, tx *sql.Tx, a account.Account) error {
	protocol := a.Protocol
	if a.Type == account.TypeKiro {
		protocol = "kiro"
	} else if !materializableProtocol(protocol) {
		// 非物化协议：清掉该账号全部模型行（物化不了的行不该留）
		_, err := tx.Exec(s.q(`DELETE FROM upstream_models WHERE account=?`), a.Name)
		return err
	}
	models := a.Models
	if len(models) == 0 {
		models = map[string]string{"*": "*"}
	}
	headers, _ := json.Marshal(a.Headers)
	overrides, _ := json.Marshal(a.Overrides)
	enabled := 0
	if a.Enabled {
		enabled = 1
	}
	keep := make([]string, 0, len(models))
	for display, native := range models {
		id := a.Name + "/" + display
		if native == "" {
			native = display
		}
		keep = append(keep, id)
		window := a.ModelLimits[display]
		if _, err := tx.Exec(s.q(`INSERT INTO upstream_models
			(id,account,display_name,protocol,native_model,base_url,headers,request_overrides,enabled,context_window,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
			account=excluded.account,display_name=excluded.display_name,protocol=excluded.protocol,
			native_model=excluded.native_model,base_url=excluded.base_url,headers=excluded.headers,
			request_overrides=excluded.request_overrides,enabled=excluded.enabled,context_window=excluded.context_window,updated_at=excluded.updated_at`),
			id, a.Name, display, protocol, native, a.BaseURL, string(headers), string(overrides), enabled, window, time.Now().Unix()); err != nil {
			return fmt.Errorf("materialize %s: %w", id, err)
		}
		if _, err := tx.Exec(s.q(`INSERT INTO model_state(upstream_model_id,updated_at) VALUES(?,?) ON CONFLICT(upstream_model_id) DO NOTHING`), id, time.Now().Unix()); err != nil {
			return err
		}
	}
	// 只删配置中已不存在的模型行；存续行走 upsert——model_state/quota_state
	// 经 FK 级联随行删除，若整账号先删后插会把熔断/冷却计数清零（启动即丢，
	// 集群下任何副本重启都会清共享库）。
	return deleteStaleModels(s, tx, a.Name, keep)
}

// deleteStaleModels 删除账号配置中已不存在的模型行（keep 分块防超占位符上限）。
func deleteStaleModels(s *Store, tx *sql.Tx, accountName string, keep []string) error {
	if len(keep) == 0 {
		_, err := tx.Exec(s.q(`DELETE FROM upstream_models WHERE account=?`), accountName)
		return err
	}
	for i := 0; i < len(keep); i += 100 {
		end := min(i+100, len(keep))
		marks := strings.TrimSuffix(strings.Repeat("?,", end-i), ",")
		args := make([]any, 0, end-i+1)
		args = append(args, accountName)
		for _, id := range keep[i:end] {
			args = append(args, id)
		}
		if _, err := tx.Exec(s.q(`DELETE FROM upstream_models WHERE account=? AND id NOT IN (`+marks+`)`), args...); err != nil {
			return err
		}
	}
	return nil
}

func materializableProtocol(protocol string) bool {
	switch protocol {
	case "anthropic", "openai-chat", "openai-responses", "codex":
		return true
	default:
		return false
	}
}

func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.DB.QueryContext(ctx, s.q(`SELECT m.id,m.account,m.display_name,m.protocol,m.native_model,m.base_url,m.headers,m.request_overrides,m.enabled,m.context_window,
		COALESCE(st.cooldown_until,0),COALESCE(st.failures,0),COALESCE(st.last_error_class,'') FROM upstream_models m LEFT JOIN model_state st ON st.upstream_model_id=m.id ORDER BY m.id`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		var m Model
		var headers, overrides string
		var enabled int
		var cooldown int64
		if err := rows.Scan(&m.ID, &m.Account, &m.DisplayName, &m.Protocol, &m.NativeModel, &m.BaseURL, &headers, &overrides, &enabled, &m.ContextWindow, &cooldown, &m.Failures, &m.LastErrorClass); err != nil {
			return nil, err
		}
		m.Enabled = enabled != 0
		if cooldown > 0 {
			m.CooldownUntil = time.Unix(cooldown, 0)
		}
		_ = json.Unmarshal([]byte(headers), &m.Headers)
		m.RequestOverrides = json.RawMessage(overrides)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetModel(ctx context.Context, id string) (*Model, error) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for i := range models {
		if models[i].ID == id {
			return &models[i], nil
		}
	}
	return nil, nil
}

func (s *Store) ApplyReport(ctx context.Context, r upstreamv1.ResultReport) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, s.q(`INSERT INTO result_reports(report_id,upstream_model_id,received_at) VALUES(?,?,?) ON CONFLICT(report_id) DO NOTHING`), r.ReportID, r.TargetID, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, tx.Commit()
	}
	if r.Outcome == "context_exceeded" {
		// 超限是请求侧问题：model_state 整行不动（failures/cooldown/last_error_class
		// 原样保留——覆盖 last_error_class 会破坏 Half-Open 试探的归类门），只记幂等 report。
		return true, tx.Commit()
	}
	// FOR UPDATE：postgres 下锁行串行化并发退避读数（多副本失败计数不丢更新）；
	// sqlite 单连接本就串行，且不支持该语法。
	lockSuffix := ""
	if s.driver == dialect.Postgres {
		lockSuffix = " FOR UPDATE"
	}
	failures := 0
	cooldown := int64(0)
	class := ""
	isFailure := r.Outcome != "normal" && r.Outcome != "retrying" && r.Outcome != "invalid_model"
	if isFailure {
		// 错误归类（Half-Open 试探资格判定）：显式冷却时刻（配额 402 等）/
		// 鉴权失效/限流不试探（试探只浪费被拒请求）；其余按 outcome 记熔断退避。
		switch {
		case !r.CooldownUntil.IsZero():
			class = "cooldown_until"
		case r.Status == 401 || r.Status == 403:
			class = "auth"
		case r.Status == 429:
			class = "rate_limit"
		default:
			class = r.Outcome
		}
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COALESCE(failures,0) FROM model_state WHERE upstream_model_id=?`+lockSuffix), r.TargetID).Scan(&failures); err != nil && err != sql.ErrNoRows {
			return false, err
		}
		failures++
		switch {
		case !r.CooldownUntil.IsZero():
			cooldown = r.CooldownUntil.Unix()
		case r.Status == 401 || r.Status == 403:
			cooldown = time.Now().Add(24 * time.Hour).Unix()
		case r.Status == 429:
			cooldown = time.Now().Add(time.Hour).Unix()
		default:
			d := time.Minute * time.Duration(1<<min(failures-1, 10))
			cooldown = time.Now().Add(d).Unix()
		}
	} else if r.Outcome == "normal" {
		failures = 0
	} else {
		var currentCooldown int64
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COALESCE(cooldown_until,0),COALESCE(failures,0) FROM model_state WHERE upstream_model_id=?`+lockSuffix), r.TargetID).Scan(&currentCooldown, &failures); err != nil && err != sql.ErrNoRows {
			return false, err
		}
		cooldown = currentCooldown
		class = r.Outcome
	}
	// 失败计数走原子自增（failures=failures+1）：行锁缺席时（如目标行尚不存在
	// 的并发插入竞争）也不丢更新；其余路径整行覆盖语义不变。
	upsert := `INSERT INTO model_state(upstream_model_id,cooldown_until,failures,last_error_class,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(upstream_model_id) DO UPDATE SET cooldown_until=excluded.cooldown_until,last_error_class=excluded.last_error_class,updated_at=excluded.updated_at`
	if isFailure {
		upsert += `,failures=model_state.failures+1`
	} else {
		upsert += `,failures=excluded.failures`
	}
	_, err = tx.ExecContext(ctx, s.q(upsert), r.TargetID, cooldown, failures, class, time.Now().Unix())
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ImportLegacy imports a compatible legacy account DB read-only and idempotently.
// 源库恒为 SQLite（mode=ro 只读打开），目标库可为任一方言。
func (s *Store) ImportLegacy(ctx context.Context, source string) error {
	abs, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	if s.path != "" {
		if same, _ := filepath.Abs(s.path); same == abs {
			return fmt.Errorf("legacy source must differ from upstream.db")
		}
	}
	var done int
	if err := s.DB.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM legacy_imports WHERE source_path=?`), abs).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		return nil
	}
	src, err := dialect.OpenSQLiteReadOnly(abs)
	if err != nil {
		return err
	}
	defer src.Close()

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := src.QueryContext(ctx, `SELECT name,type,enabled,protocol,base_url,api_key,models,headers,models_allowlist,kiro,token_state,overrides,disabled,limit_kind,cooldown_until,failures,last_failure,stats,updated_at FROM accounts ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("read legacy accounts: %w", err)
	}
	for rows.Next() {
		vals := make([]any, 19)
		ptrs := make([]any, 19)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return err
		}
		a, err := legacyAccount(vals)
		if err != nil {
			rows.Close()
			return err
		}
		if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO accounts(name,type,enabled,protocol,base_url,api_key,models,headers,models_allowlist,kiro,token_state,overrides,disabled,limit_kind,cooldown_until,failures,last_failure,stats,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET type=excluded.type,enabled=excluded.enabled,protocol=excluded.protocol,base_url=excluded.base_url,api_key=excluded.api_key,models=excluded.models,headers=excluded.headers,models_allowlist=excluded.models_allowlist,kiro=excluded.kiro,token_state=excluded.token_state,overrides=excluded.overrides,disabled=excluded.disabled,limit_kind=excluded.limit_kind,cooldown_until=excluded.cooldown_until,failures=excluded.failures,last_failure=excluded.last_failure,stats=excluded.stats,updated_at=excluded.updated_at`), vals...); err != nil {
			rows.Close()
			return fmt.Errorf("import account %s: %w", a.Name, err)
		}
		if err := materializeAccount(s, tx, a); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	usageRows, err := src.QueryContext(ctx, `SELECT rowid,account,ts,input_tokens,output_tokens,cache_read,cache_creation FROM usage_log ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("read legacy usage: %w", err)
	}
	for usageRows.Next() {
		var rowID, ts, input, output, cacheRead, cacheCreation int64
		var accountName string
		if err := usageRows.Scan(&rowID, &accountName, &ts, &input, &output, &cacheRead, &cacheCreation); err != nil {
			usageRows.Close()
			return err
		}
		if strings.TrimSpace(accountName) == "" {
			usageRows.Close()
			return fmt.Errorf("legacy usage row %d has empty account", rowID)
		}
		// RETURNING id 统一两方言（postgres 无 LastInsertId）。
		var usageID int64
		if err := tx.QueryRowContext(ctx, s.q(`INSERT INTO usage_log(account,ts,input_tokens,output_tokens,cache_read,cache_creation) VALUES(?,?,?,?,?,?) RETURNING id`),
			accountName, ts, input, output, cacheRead, cacheCreation).Scan(&usageID); err != nil {
			usageRows.Close()
			return fmt.Errorf("import usage row %d: %w", rowID, err)
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO legacy_usage_imports(source_path,source_rowid,usage_id) VALUES(?,?,?)`), abs, rowID, usageID); err != nil {
			usageRows.Close()
			return fmt.Errorf("record legacy usage row %d: %w", rowID, err)
		}
	}
	if err := usageRows.Err(); err != nil {
		usageRows.Close()
		return err
	}
	usageRows.Close()
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO legacy_imports(source_path,imported_at) VALUES(?,?)`), abs, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func legacyAccount(v []any) (account.Account, error) {
	text := func(i int) string {
		if v[i] == nil {
			return ""
		}
		s, _ := v[i].(string)
		return s
	}
	integer := func(i int) int64 {
		switch n := v[i].(type) {
		case int64:
			return n
		case int:
			return int64(n)
		default:
			return 0
		}
	}
	a := account.Account{Name: text(0), Type: text(1), Enabled: integer(2) != 0, Protocol: text(3), BaseURL: text(4), APIKey: text(5), Disabled: integer(12) != 0, LimitKind: text(13), Failures: int(integer(15))}
	if strings.TrimSpace(a.Name) == "" {
		return a, fmt.Errorf("legacy account has empty name")
	}
	if err := json.Unmarshal([]byte(text(6)), &a.Models); err != nil {
		return a, fmt.Errorf("legacy account %s has invalid models: %w", a.Name, err)
	}
	_ = json.Unmarshal([]byte(text(7)), &a.Headers)
	_ = json.Unmarshal([]byte(text(8)), &a.ModelsAllowlist)
	if raw := text(9); raw != "" {
		var k account.KiroAccount
		if err := json.Unmarshal([]byte(raw), &k); err != nil {
			return a, fmt.Errorf("legacy account %s has invalid kiro data: %w", a.Name, err)
		}
		a.Kiro = &k
	}
	if raw := text(11); raw != "" {
		var ov ir.Overrides
		if err := json.Unmarshal([]byte(raw), &ov); err != nil {
			return a, fmt.Errorf("legacy account %s has invalid overrides: %w", a.Name, err)
		}
		a.Overrides = &ov
	}
	return a, nil
}
