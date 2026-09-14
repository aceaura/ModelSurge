// migrate.go 模式一→模式二数据迁移：SQLite replay.db 只读源 → 当前库
// （design/deployment-modes.md 2.2）。行级幂等（唯一约束 upsert），
// target_cache 可重建不迁。
package relaystore

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aceaura/ModelSurge/upstream/dialect"
)

// MigrateResult 各表新迁入行数（已存在行不计）。
type MigrateResult struct {
	UserModels int64
	Groups     int64
	Members    int64
	Reports    int64
}

// Migrate 从模式一 SQLite replay.db 只读迁入当前库：user_models/
// schedule_groups/schedule_group_members/result_reports 按 FK 序幂等写入。
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

	// 1. user_models
	rows, err := src.QueryContext(ctx, `SELECT name,protocol,api_key_hash,enabled,updated_at FROM user_models ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read source user_models: %w", err)
	}
	for rows.Next() {
		var name, protocol, hash string
		var enabled int
		var updatedAt int64
		if err := rows.Scan(&name, &protocol, &hash, &enabled, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(name) == "" {
			rows.Close()
			return nil, fmt.Errorf("user model with empty name")
		}
		n, err := tx.ExecContext(ctx, s.q(`INSERT INTO user_models(name,protocol,api_key_hash,enabled,updated_at) VALUES(?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET protocol=excluded.protocol,api_key_hash=excluded.api_key_hash,enabled=excluded.enabled,updated_at=excluded.updated_at`),
			name, protocol, hash, enabled, updatedAt)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("import user model %s: %w", name, err)
		}
		if affected, _ := n.RowsAffected(); affected > 0 {
			res.UserModels += affected
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// 2. schedule_groups（FK：user_model 须已存在）
	rows, err = src.QueryContext(ctx, `SELECT id,user_model,policy_type,policy_config,updated_at FROM schedule_groups ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read source schedule_groups: %w", err)
	}
	for rows.Next() {
		var id, userModel, policyType, policyConfig string
		var updatedAt int64
		if err := rows.Scan(&id, &userModel, &policyType, &policyConfig, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(id) == "" {
			rows.Close()
			return nil, fmt.Errorf("schedule group with empty id")
		}
		n, err := tx.ExecContext(ctx, s.q(`INSERT INTO schedule_groups(id,user_model,policy_type,policy_config,updated_at) VALUES(?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET user_model=excluded.user_model,policy_type=excluded.policy_type,policy_config=excluded.policy_config,updated_at=excluded.updated_at`),
			id, userModel, policyType, policyConfig, updatedAt)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("import group %s: %w", id, err)
		}
		if affected, _ := n.RowsAffected(); affected > 0 {
			res.Groups += affected
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// 3. schedule_group_members（双唯一约束：PK(group,model) + UNIQUE(group,position)
	// → 无冲突目标的 DO NOTHING 兜两者）
	rows, err = src.QueryContext(ctx, `SELECT group_id,upstream_model_id,position FROM schedule_group_members ORDER BY group_id,position`)
	if err != nil {
		return nil, fmt.Errorf("read source members: %w", err)
	}
	for rows.Next() {
		var groupID, modelID string
		var position int
		if err := rows.Scan(&groupID, &modelID, &position); err != nil {
			rows.Close()
			return nil, err
		}
		n, err := tx.ExecContext(ctx, s.q(`INSERT INTO schedule_group_members(group_id,upstream_model_id,position) VALUES(?,?,?) ON CONFLICT DO NOTHING`),
			groupID, modelID, position)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("import member %s/%s: %w", groupID, modelID, err)
		}
		if affected, _ := n.RowsAffected(); affected > 0 {
			res.Members += affected
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// 4. result_reports 幂等账本（target_cache 可重建不迁）
	rows, err = src.QueryContext(ctx, `SELECT report_id,request_id,group_id,target_id,outcome,created_at FROM result_reports ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("read source result_reports: %w", err)
	}
	for rows.Next() {
		var reportID, requestID, groupID, targetID, outcome string
		var createdAt int64
		if err := rows.Scan(&reportID, &requestID, &groupID, &targetID, &outcome, &createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(reportID) == "" {
			rows.Close()
			return nil, fmt.Errorf("result report with empty report_id")
		}
		n, err := tx.ExecContext(ctx, s.q(`INSERT INTO result_reports(report_id,request_id,group_id,target_id,outcome,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(report_id) DO NOTHING`),
			reportID, requestID, groupID, targetID, outcome, createdAt)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("import report %s: %w", reportID, err)
		}
		if affected, _ := n.RowsAffected(); affected > 0 {
			res.Reports += affected
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}
