// Package account 账号接入：上游账号池的持久化（SQLite）与调度状态机。
// 账号身份有两个来源：yaml 种子（api-key 型，启动同步）与管理 API
// （api-key / kiro 型，运行中热增删），SQLite 记录身份与运行时状态
// （冷却/禁用/熔断/逐笔用量），token 轮转即落库。
package account

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/ir"
)

// 账号类型。
const (
	TypeAPIKey = "api-key"
	TypeKiro   = "kiro"
)

// Account 一个上游账号。api-key 型走静态字段（protocol/base_url/api_key/models），
// kiro 型凭据在 Kiro 子结构，其余字段两种类型通用。
type Account struct {
	Name    string `json:"name"`
	Type    string `json:"type"`    // "api-key" | "kiro"
	Enabled bool   `json:"enabled"` // false=人工停用（区别于熔断禁用 Disabled）

	// api-key 型
	Protocol string            `json:"protocol,omitempty"`
	BaseURL  string            `json:"base_url,omitempty"`
	APIKey   string            `json:"api_key,omitempty"`
	Models   map[string]string `json:"models,omitempty"` // canonical -> native
	// Headers api-key 型账号追加到上游请求的自定义头（如网关要求的
	// 会话/UA 头）；键为标准 MIME 键。值按凭据口径脱敏回显。
	Headers map[string]string `json:"headers,omitempty"`
	// Overrides 转发前请求参数覆盖（两种账号类型通用；nil = 透传客户端值）。
	// ir.Overrides 是 config/account/relay 共享的 IR 层类型，ir 为叶包无环。
	Overrides *ir.Overrides `json:"request_overrides,omitempty"`

	// kiro 型
	Kiro *KiroAccount `json:"kiro,omitempty"`

	// 模型白名单（两种类型通用；空=全部）
	ModelsAllowlist []string `json:"models_allowlist,omitempty"`

	// 调度运行时状态
	Disabled      bool         `json:"disabled"`       // 401/403 后禁用（凭据失效）
	LimitKind     string       `json:"limit_kind"`     // "" / "5h" / "7h" / "monthly"
	CooldownUntil time.Time    `json:"cooldown_until"` // 限流冷却截止
	Failures      int          `json:"failures"`       // 熔断连续失败数
	LastFailure   time.Time    `json:"last_failure"`
	Stats         AccountStats `json:"stats"`
	UpdatedAt     time.Time    `json:"updated_at"`

	// ResolveModel kiro 账号的运行时模型解析（懒初始化后由 Manager 注入；
	// nil 时透传，由 codec 侧兜底模型表兜底）。不参与序列化与持久化。
	ResolveModel func(model string) (native string, ok bool) `json:"-"`
}

// KiroAccount kiro 型账号的凭据与区域配置。
type KiroAccount struct {
	Source        string `json:"source"`                   // "refresh_token" | "creds_file" | "cli_db"
	RefreshToken  string `json:"refresh_token,omitempty"`  // source=refresh_token 直配
	CredsFile     string `json:"creds_file,omitempty"`     // source=creds_file 的 JSON 凭据文件路径
	CliDB         string `json:"cli_db,omitempty"`         // source=cli_db 的 kiro cli SQLite 路径
	CredsText     string `json:"creds_text,omitempty"`     // 内联凭据文本（按 source 解释；与路径字段/creds_b64 三选一）
	CredsB64      string `json:"creds_b64,omitempty"`      // 内联凭据 base64（解码后按 source 解释；可含空白）
	Region        string `json:"region,omitempty"`         // SSO 刷新区，默认 us-east-1
	APIRegion     string `json:"api_region,omitempty"`     // API 区覆盖；空则按检测链推导
	ProfileArn    string `json:"profile_arn,omitempty"`    // 可空，首次使用时自动获取回填
	WebSearch     bool   `json:"web_search,omitempty"`     // web_search 工具注入（MCP 代执行）
	FakeReasoning bool   `json:"fake_reasoning,omitempty"` // fake_reasoning 思考标签注入

	// Token 运行时 token 状态（token_state 列持久化；不随身份 JSON 走）
	Token *TokenState `json:"-"`
}

// TokenState kiro 账号的运行时 token 状态（独立列持久化，重启恢复；
// 轮转后的 refresh_token 以库内值为准，而非配置初值）。
type TokenState struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"` // 轮转后最新值
	ExpiresAt    time.Time `json:"expires_at"`
	ProfileArn   string    `json:"profile_arn"` // 自动获取结果回填
	Region       string    `json:"region"`      // 刷新区（检测结果）
	AuthType     string    `json:"auth_type"`   // KIRO_DESKTOP | AWS_SSO_OIDC
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
}

// AccountStats 账号级累计统计（调度监控口径）。
type AccountStats struct {
	Requests     int64 `json:"requests"`
	Successes    int64 `json:"successes"`
	Failures     int64 `json:"failures"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	LastUsedAt   int64 `json:"last_used_at"` // unix 秒；0=从未使用
}

// Serving 该账号能否服务某 canonical model，返回 native 模型名。
// 白名单优先；api-key 型沿用 models 映射（空=透传），kiro 型交给
// 运行时模型解析（未初始化时透传）。
func (a *Account) Serving(model string) (native string, ok bool) {
	if len(a.ModelsAllowlist) > 0 && !slices.Contains(a.ModelsAllowlist, model) {
		return "", false
	}
	if a.Type == TypeKiro {
		if a.ResolveModel != nil {
			return a.ResolveModel(model)
		}
		return model, true // 模型缓存未建：透传，编码侧由兜底表解析
	}
	if len(a.Models) == 0 {
		return model, true // 透传型
	}
	n, ok := a.Models[model]
	return n, ok
}

// Masked 返回脱敏副本：凭据只保留末 4 位（管理 API 输出统一走这里）。
func (a *Account) Masked() Account {
	m := *a
	m.APIKey = maskSecret(a.APIKey)
	for k, v := range a.Headers {
		if m.Headers == nil {
			m.Headers = map[string]string{}
		}
		m.Headers[k] = maskSecret(v)
	}
	if a.Kiro != nil {
		k := *a.Kiro
		k.RefreshToken = maskSecret(k.RefreshToken)
		k.CredsText = maskSecret(k.CredsText)
		k.CredsB64 = maskSecret(k.CredsB64)
		if k.Token != nil {
			t := *k.Token
			t.AccessToken = maskSecret(t.AccessToken)
			t.RefreshToken = maskSecret(t.RefreshToken)
			t.ClientSecret = maskSecret(t.ClientSecret)
			k.Token = &t
		}
		m.Kiro = &k
	}
	return m
}

// maskSecret 保留末 4 位；过短则全遮蔽；空串保持空（区分"未配置"与"已配置"）。
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return "****" + s[len(s)-4:]
}

// Store 账号存储。时间一律 unix 秒；driver 决定占位符重写与方言分支。
type Store struct {
	db     *sql.DB
	driver string
	ownsDB bool
}

// schema 建表 DDL：INTEGER→BIGINT 仅影响 postgres（8 字节时间戳），
// sqlite 的 BIGINT 亲和性与 INTEGER 等价；usage_log.id 经方言选自增子句。
func schema(driver string) string {
	return `
CREATE TABLE IF NOT EXISTS accounts (
	name             TEXT PRIMARY KEY,
	type             TEXT NOT NULL DEFAULT 'api-key',
	enabled          INTEGER NOT NULL DEFAULT 1,
	protocol         TEXT NOT NULL DEFAULT '',
	base_url         TEXT NOT NULL DEFAULT '',
	api_key          TEXT NOT NULL DEFAULT '',
	models           TEXT NOT NULL DEFAULT '{}',
	headers          TEXT NOT NULL DEFAULT '',
	models_allowlist TEXT NOT NULL DEFAULT '',
	kiro             TEXT NOT NULL DEFAULT '',
	token_state      TEXT NOT NULL DEFAULT '',
	overrides        TEXT NOT NULL DEFAULT '',
	disabled         INTEGER NOT NULL DEFAULT 0,
	limit_kind       TEXT NOT NULL DEFAULT '',
	cooldown_until   BIGINT NOT NULL DEFAULT 0,
	failures         INTEGER NOT NULL DEFAULT 0,
	last_failure     BIGINT NOT NULL DEFAULT 0,
	stats            TEXT NOT NULL DEFAULT '{}',
	updated_at       BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS usage_log (
	id              ` + dialect.IdentityPK(driver) + `,
	account         TEXT NOT NULL,
	ts              BIGINT NOT NULL,
	input_tokens    BIGINT NOT NULL DEFAULT 0,
	output_tokens   BIGINT NOT NULL DEFAULT 0,
	cache_read      BIGINT NOT NULL DEFAULT 0,
	cache_creation  BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_account_ts ON usage_log(account, ts);
`
}

// v2 迁移：v1 库缺列时补齐（ALTER 增列带默认值，v1 行自动获得默认值）。
var v2Columns = []string{
	`ALTER TABLE accounts ADD COLUMN type TEXT NOT NULL DEFAULT 'api-key'`,
	`ALTER TABLE accounts ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE accounts ADD COLUMN models_allowlist TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN kiro TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN token_state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN overrides TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN failures INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE accounts ADD COLUMN last_failure INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE accounts ADD COLUMN stats TEXT NOT NULL DEFAULT '{}'`,
}

// v3 迁移：账号级自定义请求头（如 OpenCode Go 网关的 x-opencode-session）。
var v3Columns = []string{
	`ALTER TABLE accounts ADD COLUMN headers TEXT NOT NULL DEFAULT ''`,
}

// Open 打开（必要时创建）数据库并建表/迁移。driver 见 dialect 包；
// sqlite 的 dsn 为裸文件路径（自动补 pragma DSN），postgres 为连接串。
func Open(driver, dsn string) (*Store, error) {
	if !dialect.Valid(driver) {
		return nil, fmt.Errorf("account: unsupported driver %q", driver)
	}
	db, err := dialect.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("account: open: %w", err)
	}
	store, err := OpenDB(db, driver)
	if err != nil {
		db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// OpenDB initializes the account schema on a caller-owned connection pool.
// 方言随池一并传入（占位符重写与方言分支）；Close 不关池。
func OpenDB(db *sql.DB, driver string) (*Store, error) {
	if !dialect.Valid(driver) {
		return nil, fmt.Errorf("account: unsupported driver %q", driver)
	}
	if _, err := db.Exec(schema(driver)); err != nil {
		return nil, fmt.Errorf("account: migrate: %w", err)
	}
	if err := migrateColumns(db, driver, "v2", v2Columns); err != nil {
		return nil, fmt.Errorf("account: migrate v2: %w", err)
	}
	if err := migrateColumns(db, driver, "v3", v3Columns); err != nil {
		return nil, fmt.Errorf("account: migrate v3: %w", err)
	}
	return &Store{db: db, driver: driver}, nil
}

// q 按方言重写占位符（postgres ? → $n）。
func (s *Store) q(query string) string { return dialect.Rebind(s.driver, query) }

// migrateColumns 检测缺列并 ALTER 补齐；列已存在时为 no-op。
func migrateColumns(db *sql.DB, driver, ver string, columns []string) error {
	have, err := tableColumns(db, driver, "accounts")
	if err != nil {
		return err
	}
	for _, stmt := range columns {
		col := stmt[len(`ALTER TABLE accounts ADD COLUMN `):]
		col = strings.Fields(col)[0]
		if have[col] {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", col, err)
		}
	}
	return nil
}

// tableColumns 列出表已有列：sqlite 走 PRAGMA table_info，postgres 走
// information_schema（列名小写）。
func tableColumns(db *sql.DB, driver, table string) (map[string]bool, error) {
	var query string
	var args []any
	if driver == dialect.Postgres {
		query = `SELECT column_name FROM information_schema.columns WHERE table_name=$1`
		args = []any{table}
	} else {
		query = `PRAGMA table_info(` + table + `)`
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

func (s *Store) Close() error {
	if !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

// DB exposes the shared connection pool for store composition and transactions.
func (s *Store) DB() *sql.DB { return s.db }

// accountSelect 全字段读取（与 scanAccount 对应）。
const accountSelect = `SELECT name, type, enabled, protocol, base_url, api_key, models,
	headers, models_allowlist, kiro, token_state, overrides, disabled, limit_kind, cooldown_until,
	failures, last_failure, stats, updated_at FROM accounts`

// scanAccount 一行 -> Account。
func scanAccount(s interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var typ, protocol, baseURL, apiKey, models, headers, allowlist, kiroJSON, tokenJSON, overridesJSON, statsJSON string
	var enabled, disabled, failures int
	var cooldown, lastFailure, updatedAt int64
	if err := s.Scan(&a.Name, &typ, &enabled, &protocol, &baseURL, &apiKey, &models,
		&headers, &allowlist, &kiroJSON, &tokenJSON, &overridesJSON, &disabled, &a.LimitKind, &cooldown,
		&failures, &lastFailure, &statsJSON, &updatedAt); err != nil {
		return a, err
	}
	a.Type = typ
	a.Enabled = enabled != 0
	a.Protocol, a.BaseURL, a.APIKey = protocol, baseURL, apiKey
	_ = json.Unmarshal([]byte(models), &a.Models)
	_ = json.Unmarshal([]byte(headers), &a.Headers)
	_ = json.Unmarshal([]byte(allowlist), &a.ModelsAllowlist)
	_ = json.Unmarshal([]byte(statsJSON), &a.Stats)
	if overridesJSON != "" {
		var ov ir.Overrides
		if err := json.Unmarshal([]byte(overridesJSON), &ov); err == nil {
			a.Overrides = &ov
		}
	}
	if kiroJSON != "" {
		var k KiroAccount
		if err := json.Unmarshal([]byte(kiroJSON), &k); err == nil {
			a.Kiro = &k
		}
	}
	if tokenJSON != "" && a.Kiro != nil {
		var ts TokenState
		if err := json.Unmarshal([]byte(tokenJSON), &ts); err == nil {
			a.Kiro.Token = &ts
		}
	}
	a.Disabled = disabled != 0
	a.Failures = failures
	if cooldown > 0 {
		a.CooldownUntil = time.Unix(cooldown, 0)
	}
	if lastFailure > 0 {
		a.LastFailure = time.Unix(lastFailure, 0)
	}
	if updatedAt > 0 {
		a.UpdatedAt = time.Unix(updatedAt, 0)
	}
	return a, nil
}

// ListAccounts 返回全部账号：sqlite 按插入顺序（rowid 保序）；
// postgres 无 rowid，按名排序（顺序仅为展示/遍历便利，无功能依赖）。
func (s *Store) ListAccounts() ([]Account, error) {
	orderBy := ` ORDER BY rowid`
	if s.driver == dialect.Postgres {
		orderBy = ` ORDER BY name`
	}
	rows, err := s.db.Query(s.q(accountSelect + orderBy))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount 按名取账号。
func (s *Store) GetAccount(name string) (*Account, error) {
	var a Account
	row := s.db.QueryRow(s.q(accountSelect+` WHERE name=?`), name)
	acc, err := scanAccount(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a = acc
	return &a, nil
}

// SetCooldown 标记账号限流冷却。
func (s *Store) SetCooldown(name, kind string, until time.Time) error {
	_, err := s.db.Exec(s.q(`UPDATE accounts SET limit_kind=?, cooldown_until=?, updated_at=? WHERE name=?`),
		kind, until.Unix(), time.Now().Unix(), name)
	return err
}

// SetDisabled 标记/解除账号禁用（key 失效时自动禁用，换 key 后恢复）。
func (s *Store) SetDisabled(name string, disabled bool) error {
	d := 0
	if disabled {
		d = 1
	}
	if _, err := s.db.Exec(s.q(`UPDATE accounts SET disabled=?, updated_at=? WHERE name=?`),
		d, time.Now().Unix(), name); err != nil {
		return err
	}
	if !disabled { // 恢复时清掉冷却
		_, err := s.db.Exec(s.q(`UPDATE accounts SET limit_kind='', cooldown_until=0 WHERE name=?`), name)
		return err
	}
	return nil
}

// Usage 一笔请求的用量（与 ir.Usage 字段对应，避免 account 依赖 ir）。
type Usage struct {
	InputTokens   int64
	OutputTokens  int64
	CacheRead     int64
	CacheCreation int64
}

// InsertUsage 记一笔真实用量（估算值不入库）。
func (s *Store) InsertUsage(account string, ts time.Time, u Usage) error {
	_, err := s.db.Exec(s.q(`INSERT INTO usage_log (account, ts, input_tokens, output_tokens, cache_read, cache_creation)
		VALUES (?,?,?,?,?,?)`), account, ts.Unix(), u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheCreation)
	return err
}

// WindowUsage 某账号自 since 起的用量合计。
func (s *Store) WindowUsage(account string, since time.Time) (Usage, error) {
	var u Usage
	err := s.db.QueryRow(s.q(`SELECT
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_creation),0)
		FROM usage_log WHERE account=? AND ts>=?`), account, since.Unix()).
		Scan(&u.InputTokens, &u.OutputTokens, &u.CacheRead, &u.CacheCreation)
	return u, err
}
