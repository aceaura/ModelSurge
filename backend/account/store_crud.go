package account

import (
	"encoding/json"
	"fmt"
	"time"
)

// InsertAccount 落库一个新账号（管理 API 创建路径）。
// 运行时状态（token/冷却/熔断/统计）从零开始。
func (s *Store) InsertAccount(a *Account) error {
	models, _ := json.Marshal(a.Models)
	allowlist, _ := json.Marshal(a.ModelsAllowlist)
	kiro := encodeKiro(a.Kiro)
	enabled, disabled := 0, 0
	if a.Enabled {
		enabled = 1
	}
	if a.Disabled {
		disabled = 1
	}
	if _, err := s.db.Exec(`INSERT INTO accounts
		(name, type, enabled, protocol, base_url, api_key, models, models_allowlist, kiro, disabled, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.Name, a.Type, enabled, a.Protocol, a.BaseURL, a.APIKey,
		string(models), string(allowlist), kiro, disabled, time.Now().Unix()); err != nil {
		return fmt.Errorf("account: insert %q: %w", a.Name, err)
	}
	return nil
}

// UpdateAccount 更新账号身份字段（凭据/模型/白名单/enabled；管理 API 路径）。
// 运行时状态（disabled 除外）/token_state/统计/冷却不动。
func (s *Store) UpdateAccount(a *Account) error {
	models, _ := json.Marshal(a.Models)
	allowlist, _ := json.Marshal(a.ModelsAllowlist)
	kiro := encodeKiro(a.Kiro)
	enabled := 0
	if a.Enabled {
		enabled = 1
	}
	res, err := s.db.Exec(`UPDATE accounts SET
		type=?, enabled=?, protocol=?, base_url=?, api_key=?, models=?,
		models_allowlist=?, kiro=?, updated_at=? WHERE name=?`,
		a.Type, enabled, a.Protocol, a.BaseURL, a.APIKey, string(models),
		string(allowlist), kiro, time.Now().Unix(), a.Name)
	if err != nil {
		return fmt.Errorf("account: update %q: %w", a.Name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("account: update %q: not found", a.Name)
	}
	return nil
}

// DeleteAccount 删除账号行（usage_log 历史保留）。
func (s *Store) DeleteAccount(name string) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE name=?`, name)
	return err
}

// SaveTokenState 持久化 kiro 账号的 token 状态（刷新轮转即调）。
func (s *Store) SaveTokenState(name string, ts *TokenState) error {
	if ts == nil {
		ts = &TokenState{}
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE accounts SET token_state=?, updated_at=? WHERE name=?`,
		string(b), time.Now().Unix(), name)
	return err
}

// SaveStats 持久化账号统计（成功/失败记账）。
func (s *Store) SaveStats(name string, stats AccountStats) error {
	b, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE accounts SET stats=?, updated_at=? WHERE name=?`,
		string(b), time.Now().Unix(), name)
	return err
}

// SetFailures 持久化熔断计数（连续失败数 + 最近失败时刻）。
func (s *Store) SetFailures(name string, failures int, at time.Time) error {
	_, err := s.db.Exec(`UPDATE accounts SET failures=?, last_failure=?, updated_at=? WHERE name=?`,
		failures, at.Unix(), time.Now().Unix(), name)
	return err
}

// encodeKiro KiroAccount -> JSON（Token 不随身份列走，存 token_state 列）。
func encodeKiro(k *KiroAccount) string {
	if k == nil {
		return ""
	}
	b, _ := json.Marshal(k)
	return string(b)
}
