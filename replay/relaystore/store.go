// Package relaystore exclusively owns relay.db scheduling and client-facing model state.
package relaystore

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }
type UserModel struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	APIKey   string `json:"api_key,omitempty"`
	Enabled  bool   `json:"enabled"`
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

const schema = `
CREATE TABLE IF NOT EXISTS user_models(name TEXT PRIMARY KEY,protocol TEXT NOT NULL,api_key_hash TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS schedule_groups(id TEXT PRIMARY KEY,user_model TEXT NOT NULL UNIQUE REFERENCES user_models(name),policy_type TEXT NOT NULL,policy_config TEXT NOT NULL DEFAULT '{}',updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS schedule_group_members(group_id TEXT NOT NULL REFERENCES schedule_groups(id) ON DELETE CASCADE,upstream_model_id TEXT NOT NULL,position INTEGER NOT NULL,PRIMARY KEY(group_id,upstream_model_id),UNIQUE(group_id,position));
CREATE TABLE IF NOT EXISTS target_cache(group_id TEXT PRIMARY KEY REFERENCES schedule_groups(id) ON DELETE CASCADE,upstream_model_id TEXT NOT NULL,last_result TEXT NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS result_reports(report_id TEXT PRIMARY KEY,request_id TEXT NOT NULL DEFAULT '',group_id TEXT NOT NULL,target_id TEXT NOT NULL,outcome TEXT NOT NULL,created_at INTEGER NOT NULL);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("relaystore: migrate: %w", err)
	}
	return &Store{DB: db}, nil
}
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
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_models(name,protocol,api_key_hash,enabled,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(name) DO NOTHING`, m, "auto", hash, 1, now); err != nil {
			return err
		}
		gid := "compat/" + m
		if _, err = tx.ExecContext(ctx, `INSERT INTO schedule_groups(id,user_model,policy_type,policy_config,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, gid, m, "preset", "{}", now); err != nil {
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM schedule_group_members WHERE group_id=?`, groupID); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err = tx.ExecContext(ctx, `INSERT INTO schedule_group_members(group_id,upstream_model_id,position)VALUES(?,?,?)`, groupID, id, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) GroupForModel(ctx context.Context, model string) (*Group, error) {
	var g Group
	var cacheUpdated int64
	err := s.DB.QueryRowContext(ctx, `SELECT g.id,g.user_model,g.policy_type,g.policy_config,COALESCE(c.upstream_model_id,''),COALESCE(c.last_result,''),COALESCE(c.updated_at,0) FROM schedule_groups g LEFT JOIN target_cache c ON c.group_id=g.id JOIN user_models u ON u.name=g.user_model WHERE g.user_model=? AND u.enabled=1`, model).Scan(&g.ID, &g.UserModel, &g.PolicyType, &g.PolicyConfig, &g.CachedTarget, &g.LastResult, &cacheUpdated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if cacheUpdated > 0 {
		g.CacheUpdated = time.Unix(cacheUpdated, 0)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT upstream_model_id FROM schedule_group_members WHERE group_id=? ORDER BY position`, g.ID)
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
	rows, err := s.DB.QueryContext(ctx, `SELECT name FROM user_models WHERE enabled=1 ORDER BY name`)
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

func (s *Store) Authenticate(ctx context.Context, model, protocol, key string) (configured bool, ok bool, err error) {
	var want, storedProtocol string
	err = s.DB.QueryRowContext(ctx, `SELECT protocol,api_key_hash FROM user_models WHERE name=? AND enabled=1`, model).Scan(&storedProtocol, &want)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if storedProtocol != "" && storedProtocol != "auto" && storedProtocol != protocol {
		return true, false, nil
	}
	got := HashAPIKey(key)
	if len(want) != len(got) {
		return true, false, nil
	}
	return true, subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1, nil
}
func (s *Store) SetCache(ctx context.Context, groupID, targetID, result string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO target_cache(group_id,upstream_model_id,last_result,updated_at)VALUES(?,?,?,?) ON CONFLICT(group_id) DO UPDATE SET upstream_model_id=excluded.upstream_model_id,last_result=excluded.last_result,updated_at=excluded.updated_at`, groupID, targetID, result, time.Now().Unix())
	return err
}

func (s *Store) InvalidateCache(ctx context.Context, groupID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM target_cache WHERE group_id=?`, groupID)
	return err
}

// ApplyReport records reportID and updates the cache atomically. Duplicate report IDs are no-ops.
func (s *Store) ApplyReport(ctx context.Context, reportID, requestID, groupID, targetID, outcome string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO result_reports(report_id,request_id,group_id,target_id,outcome,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(report_id) DO NOTHING`, reportID, requestID, groupID, targetID, outcome, time.Now().Unix())
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO target_cache(group_id,upstream_model_id,last_result,updated_at)VALUES(?,?,?,?) ON CONFLICT(group_id) DO UPDATE SET upstream_model_id=excluded.upstream_model_id,last_result=excluded.last_result,updated_at=excluded.updated_at`, groupID, targetID, cacheResult, time.Now().Unix()); err != nil {
		return false, err
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
	_, err := s.DB.ExecContext(ctx, `INSERT INTO user_models(name,protocol,api_key_hash,enabled,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET protocol=excluded.protocol,api_key_hash=CASE WHEN excluded.api_key_hash='' THEN user_models.api_key_hash ELSE excluded.api_key_hash END,enabled=excluded.enabled,updated_at=excluded.updated_at`, m.Name, m.Protocol, HashAPIKey(m.APIKey), enabled, time.Now().Unix())
	return err
}

func (s *Store) ListUserModels(ctx context.Context) ([]UserModel, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT name,protocol,enabled FROM user_models ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserModel
	for rows.Next() {
		var m UserModel
		var enabled int
		if err := rows.Scan(&m.Name, &m.Protocol, &enabled); err != nil {
			return nil, err
		}
		m.Enabled = enabled != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUserModel(ctx context.Context, name string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM user_models WHERE name=?`, name)
	return err
}

func (s *Store) PutGroup(ctx context.Context, g Group) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO schedule_groups(id,user_model,policy_type,policy_config,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET user_model=excluded.user_model,policy_type=excluded.policy_type,policy_config=excluded.policy_config,updated_at=excluded.updated_at`, g.ID, g.UserModel, g.PolicyType, g.PolicyConfig, time.Now().Unix())
	return err
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_model,policy_type,policy_config FROM schedule_groups ORDER BY id`)
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
	_, err := s.DB.ExecContext(ctx, `DELETE FROM schedule_groups WHERE id=?`, id)
	return err
}
