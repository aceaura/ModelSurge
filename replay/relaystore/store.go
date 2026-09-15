// Package relaystore exclusively owns relay.db scheduling and client-facing model state.
package relaystore

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/redisx"
)

type Store struct {
	DB     *sql.DB
	driver string
	// path sqlite 模式下的库路径（Migrate 源路径防混用）；postgres 为空。
	path string
	// Redis 可选热态层（鉴权缓存）；nil = 纯 DB 路径（模式一现行为）。
	Redis *redisx.Client
}
type UserModel struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	APIKey   string `json:"api_key,omitempty"`
	Enabled  bool   `json:"enabled"`
	// CompressModel 压缩备用 user model 名（空串=关闭压缩回退）。
	CompressModel string `json:"compress_model,omitempty"`
}
type Group struct {
	ID           string    `json:"id"`
	UserModel    string    `json:"user_model"`
	PolicyType   string    `json:"policy_type"`
	PolicyConfig string    `json:"policy_config"`
	Members      []string  `json:"members"`
	CachedTarget string    `json:"cached_target,omitempty"`
	LastResult   string    `json:"last_result,omitempty"`
	CacheUpdated time.Time `json:"cache_updated,omitempty"`
}

// schema 时间戳列用 BIGINT（postgres 8 字节；sqlite 亲和性与 INTEGER 等价）。
const schema = `
CREATE TABLE IF NOT EXISTS user_models(name TEXT PRIMARY KEY,protocol TEXT NOT NULL,api_key_hash TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS schedule_groups(id TEXT PRIMARY KEY,user_model TEXT NOT NULL UNIQUE REFERENCES user_models(name),policy_type TEXT NOT NULL,policy_config TEXT NOT NULL DEFAULT '{}',updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS schedule_group_members(group_id TEXT NOT NULL REFERENCES schedule_groups(id) ON DELETE CASCADE,upstream_model_id TEXT NOT NULL,position INTEGER NOT NULL,PRIMARY KEY(group_id,upstream_model_id),UNIQUE(group_id,position));
CREATE TABLE IF NOT EXISTS target_cache(group_id TEXT PRIMARY KEY REFERENCES schedule_groups(id) ON DELETE CASCADE,upstream_model_id TEXT NOT NULL,last_result TEXT NOT NULL,updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS result_reports(report_id TEXT PRIMARY KEY,request_id TEXT NOT NULL DEFAULT '',group_id TEXT NOT NULL,target_id TEXT NOT NULL,outcome TEXT NOT NULL,created_at BIGINT NOT NULL);
`

func Open(driver, dsn string) (*Store, error) {
	if !dialect.Valid(driver) {
		return nil, fmt.Errorf("relaystore: unsupported driver %q", driver)
	}
	db, err := dialect.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("relaystore: migrate: %w", err)
	}
	// 存量库补列（CREATE IF NOT EXISTS 不改已存在表；「列已存在」忽略）。
	if _, err = db.Exec(`ALTER TABLE user_models ADD COLUMN compress_model TEXT NOT NULL DEFAULT ''`); err != nil && !columnExists(err) {
		db.Close()
		return nil, fmt.Errorf("relaystore: migrate compress_model: %w", err)
	}
	return &Store{DB: db, driver: driver, path: dialect.SQLitePath(driver, dsn)}, nil
}

// columnExists 判定 ALTER ADD COLUMN 的「列已存在」错误（sqlite/pg 方言串不同）。
func columnExists(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column name") || strings.Contains(msg, "already exists")
}

// q 按方言重写占位符（postgres ? → $n）。
func (s *Store) q(query string) string { return dialect.Rebind(s.driver, query) }

func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Bootstrap(ctx context.Context, apiKey string, models []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	hash := HashAPIKey(apiKey)
	for _, m := range models {
		if m == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO user_models(name,protocol,api_key_hash,enabled,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(name) DO NOTHING`), m, "auto", hash, 1, now); err != nil {
			return err
		}
		gid := "compat/" + m
		if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO schedule_groups(id,user_model,policy_type,policy_config,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`), gid, m, "preset", "{}", now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) AddMembers(ctx context.Context, groupID string, ids []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, s.q(`DELETE FROM schedule_group_members WHERE group_id=?`), groupID); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO schedule_group_members(group_id,upstream_model_id,position)VALUES(?,?,?)`), groupID, id, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) GroupForModel(ctx context.Context, model string) (*Group, error) {
	var g Group
	var cacheUpdated int64
	err := s.DB.QueryRowContext(ctx, s.q(`SELECT g.id,g.user_model,g.policy_type,g.policy_config,COALESCE(c.upstream_model_id,''),COALESCE(c.last_result,''),COALESCE(c.updated_at,0) FROM schedule_groups g LEFT JOIN target_cache c ON c.group_id=g.id JOIN user_models u ON u.name=g.user_model WHERE g.user_model=? AND u.enabled=1`), model).Scan(&g.ID, &g.UserModel, &g.PolicyType, &g.PolicyConfig, &g.CachedTarget, &g.LastResult, &cacheUpdated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if cacheUpdated > 0 {
		g.CacheUpdated = time.Unix(cacheUpdated, 0)
	}
	rows, err := s.DB.QueryContext(ctx, s.q(`SELECT upstream_model_id FROM schedule_group_members WHERE group_id=? ORDER BY position`), g.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		g.Members = append(g.Members, id)
	}
	return &g, rows.Err()
}
func (s *Store) Models(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, s.q(`SELECT name FROM user_models WHERE enabled=1 ORDER BY name`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// authCacheTTL 鉴权缓存 TTL（设计 2.4：60s 兜底，写路径主动失效）。
const authCacheTTL = 60 * time.Second

// authCacheKey 按 user model 名取缓存键（值 = "protocol|api_key_hash|compress_model"，
// 空 protocol 段代表「未配置/禁用」负缓存）。
func authCacheKey(model string) string { return "auth:" + model }

// Authenticate 校验客户端 API key。Redis 热态旁路：命中则免 DB 直读，
// 本地完成协议匹配与常数时间比较；未命中走 DB 并回填。
// Redis 未配置或降级 = 纯 DB 路径（现行为）。
// compressOf 非空 = Agent 内部压缩调用（dispatch 目标为 compress_model）：
// 跳过 key 与协议校验放行（信任 Agent 已对原请求完成鉴权），但模型存在且
// 启用的判定不豁免。compressModel 返回该 user model 配置的压缩备用模型名。
func (s *Store) Authenticate(ctx context.Context, model, protocol, key, compressOf string) (configured bool, ok bool, compressModel string, err error) {
	if s.Redis != nil {
		if v, found, rerr := s.Redis.Get(ctx, authCacheKey(model)); rerr == nil && found {
			if v == "" {
				return false, false, "", nil
			}
			var storedProtocol, want, cachedCompress string
			if parts := strings.SplitN(v, "|", 3); len(parts) > 1 {
				storedProtocol, want = parts[0], parts[1]
				if len(parts) > 2 {
					cachedCompress = parts[2]
				}
			} else {
				storedProtocol = parts[0]
			}
			if compressOf == "" && storedProtocol != "" && storedProtocol != "auto" && storedProtocol != protocol {
				return true, false, cachedCompress, nil
			}
			if compressOf != "" {
				return true, true, cachedCompress, nil
			}
			got := HashAPIKey(key)
			if len(want) != len(got) {
				return true, false, cachedCompress, nil
			}
			return true, subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1, cachedCompress, nil
		}
	}
	var want, storedProtocol, storedCompress string
	err = s.DB.QueryRowContext(ctx, s.q(`SELECT protocol,api_key_hash,compress_model FROM user_models WHERE name=? AND enabled=1`), model).Scan(&storedProtocol, &want, &storedCompress)
	if err == sql.ErrNoRows {
		s.cacheAuth(ctx, model, "", "")
		return false, false, "", nil
	}
	if err != nil {
		return false, false, "", err
	}
	s.cacheAuth(ctx, model, storedProtocol, want, storedCompress)
	if compressOf != "" {
		return true, true, storedCompress, nil
	}
	if storedProtocol != "" && storedProtocol != "auto" && storedProtocol != protocol {
		return true, false, storedCompress, nil
	}
	got := HashAPIKey(key)
	if len(want) != len(got) {
		return true, false, storedCompress, nil
	}
	return true, subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1, storedCompress, nil
}

// cacheAuth 回填鉴权缓存（nil/失败静默：TTL 兜底，不影响正确性）。
// 可变参数末位为 compress_model（省略 = 空串；旧调用兼容）。
func (s *Store) cacheAuth(ctx context.Context, model, protocol, hash string, compress ...string) {
	if s.Redis == nil {
		return
	}
	cm := ""
	if len(compress) > 0 {
		cm = compress[0]
	}
	val := ""
	if protocol != "" || hash != "" {
		val = protocol + "|" + hash + "|" + cm
	}
	_ = s.Redis.SetEx(ctx, authCacheKey(model), val, authCacheTTL)
}
func (s *Store) SetCache(ctx context.Context, groupID, targetID, result string) error {
	_, err := s.DB.ExecContext(ctx, s.q(`INSERT INTO target_cache(group_id,upstream_model_id,last_result,updated_at)VALUES(?,?,?,?) ON CONFLICT(group_id) DO UPDATE SET upstream_model_id=excluded.upstream_model_id,last_result=excluded.last_result,updated_at=excluded.updated_at`), groupID, targetID, result, time.Now().Unix())
	return err
}

func (s *Store) InvalidateCache(ctx context.Context, groupID string) error {
	_, err := s.DB.ExecContext(ctx, s.q(`DELETE FROM target_cache WHERE group_id=?`), groupID)
	return err
}

// ApplyReport records reportID and updates the cache atomically. Duplicate report IDs are no-ops.
func (s *Store) ApplyReport(ctx context.Context, reportID, requestID, groupID, targetID, outcome string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, s.q(`INSERT INTO result_reports(report_id,request_id,group_id,target_id,outcome,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(report_id) DO NOTHING`), reportID, requestID, groupID, targetID, outcome, time.Now().Unix())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, tx.Commit()
	}
	cacheResult := "abnormal"
	if outcome == "normal" {
		cacheResult = "normal"
	}
	// 超限是请求侧问题：target_cache 保持原值，不把无恙目标改写成 abnormal。
	if outcome != "context_exceeded" {
		if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO target_cache(group_id,upstream_model_id,last_result,updated_at)VALUES(?,?,?,?) ON CONFLICT(group_id) DO UPDATE SET upstream_model_id=excluded.upstream_model_id,last_result=excluded.last_result,updated_at=excluded.updated_at`), groupID, targetID, cacheResult, time.Now().Unix()); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) PutUserModel(ctx context.Context, m UserModel) error {
	enabled := 0
	if m.Enabled {
		enabled = 1
	}
	_, err := s.DB.ExecContext(ctx, s.q(`INSERT INTO user_models(name,protocol,api_key_hash,enabled,compress_model,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET protocol=excluded.protocol,api_key_hash=CASE WHEN excluded.api_key_hash='' THEN user_models.api_key_hash ELSE excluded.api_key_hash END,enabled=excluded.enabled,compress_model=excluded.compress_model,updated_at=excluded.updated_at`), m.Name, m.Protocol, HashAPIKey(m.APIKey), enabled, m.CompressModel, time.Now().Unix())
	if err == nil {
		s.invalidateAuth(ctx, m.Name)
	}
	return err
}

func (s *Store) ListUserModels(ctx context.Context) ([]UserModel, error) {
	rows, err := s.DB.QueryContext(ctx, s.q(`SELECT name,protocol,enabled,compress_model FROM user_models ORDER BY name`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserModel
	for rows.Next() {
		var m UserModel
		var enabled int
		if err := rows.Scan(&m.Name, &m.Protocol, &enabled, &m.CompressModel); err != nil {
			return nil, err
		}
		m.Enabled = enabled != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUserModel(ctx context.Context, name string) error {
	_, err := s.DB.ExecContext(ctx, s.q(`DELETE FROM user_models WHERE name=?`), name)
	if err == nil {
		s.invalidateAuth(ctx, name)
	}
	return err
}

// invalidateAuth 主动失效鉴权缓存（设计 2.4：写后失效，TTL 60s 兜底）。
func (s *Store) invalidateAuth(ctx context.Context, name string) {
	if s.Redis != nil {
		_ = s.Redis.Del(ctx, authCacheKey(name))
	}
}

func (s *Store) PutGroup(ctx context.Context, g Group) error {
	_, err := s.DB.ExecContext(ctx, s.q(`INSERT INTO schedule_groups(id,user_model,policy_type,policy_config,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET user_model=excluded.user_model,policy_type=excluded.policy_type,policy_config=excluded.policy_config,updated_at=excluded.updated_at`), g.ID, g.UserModel, g.PolicyType, g.PolicyConfig, time.Now().Unix())
	return err
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.DB.QueryContext(ctx, s.q(`SELECT id,user_model,policy_type,policy_config FROM schedule_groups ORDER BY id`))
	if err != nil {
		return nil, err
	}
	var base []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.UserModel, &g.PolicyType, &g.PolicyConfig); err != nil {
			rows.Close()
			return nil, err
		}
		base = append(base, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]Group, 0, len(base))
	for _, g := range base {
		full, err := s.GroupForModel(ctx, g.UserModel)
		if err != nil {
			return nil, err
		}
		if full != nil {
			g = *full
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, s.q(`DELETE FROM schedule_groups WHERE id=?`), id)
	return err
}
