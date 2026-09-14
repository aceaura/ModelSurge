package agentstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/upstream/dialect"
)

type Store struct {
	DB     *sql.DB
	driver string
}

type RequestLog struct {
	RequestID       string
	At              time.Time
	InboundProtocol string
	UserModel       string
	TargetID        string
	Result          string
	Usage           replayv1.Usage
}

type OutboxItem struct {
	ID       int64
	Report   replayv1.ResultReport
	Attempts int
}

func Open(driver, dsn string) (*Store, error) {
	if !dialect.Valid(driver) {
		return nil, fmt.Errorf("agentstore: unsupported driver %q", driver)
	}
	db, err := dialect.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	// outbox id 经方言选自增子句（postgres IDENTITY，sqlite AUTOINCREMENT 防 rowid 复用）。
	if _, err = db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS request_log (
 request_id TEXT PRIMARY KEY, at TEXT NOT NULL, inbound_protocol TEXT NOT NULL,
 user_model TEXT NOT NULL, target_id TEXT NOT NULL, result TEXT NOT NULL, usage_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS report_outbox (
 id %s, report_id TEXT NOT NULL UNIQUE, report_json TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT NOT NULL, created_at TEXT NOT NULL
);`, dialect.IdentityPK(driver))); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, driver: driver}, nil
}

// q 按方言重写占位符（postgres ? → $n）。
func (s *Store) q(query string) string { return dialect.Rebind(s.driver, query) }

func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) LogRequest(ctx context.Context, v RequestLog) error {
	u, _ := json.Marshal(v.Usage)
	_, err := s.DB.ExecContext(ctx, s.q(`INSERT INTO request_log(request_id,at,inbound_protocol,user_model,target_id,result,usage_json) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(request_id) DO UPDATE SET at=excluded.at,inbound_protocol=excluded.inbound_protocol,user_model=excluded.user_model,target_id=excluded.target_id,result=excluded.result,usage_json=excluded.usage_json`),
		v.RequestID, v.At.UTC().Format(time.RFC3339Nano), v.InboundProtocol, v.UserModel, v.TargetID, v.Result, string(u))
	return err
}
func (s *Store) Enqueue(ctx context.Context, r replayv1.ResultReport) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.DB.ExecContext(ctx, s.q(`INSERT INTO report_outbox(report_id,report_json,next_attempt_at,created_at) VALUES(?,?,?,?) ON CONFLICT(report_id) DO NOTHING`), r.ReportID, string(b), now, now)
	return err
}
func (s *Store) Due(ctx context.Context, limit int) ([]OutboxItem, error) {
	rows, err := s.DB.QueryContext(ctx, s.q(`SELECT id,report_json,attempts FROM report_outbox WHERE next_attempt_at<=? ORDER BY id LIMIT ?`), time.Now().UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var v OutboxItem
		var raw string
		if err := rows.Scan(&v.ID, &raw, &v.Attempts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &v.Report); err != nil {
			return nil, fmt.Errorf("decode outbox %d: %w", v.ID, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Ack(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, s.q(`DELETE FROM report_outbox WHERE id=?`), id)
	return err
}
func (s *Store) Retry(ctx context.Context, id int64, attempts int) error {
	delay := time.Duration(1<<min(attempts, 6)) * time.Second
	_, err := s.DB.ExecContext(ctx, s.q(`UPDATE report_outbox SET attempts=?,next_attempt_at=? WHERE id=?`), attempts, time.Now().Add(delay).UTC().Format(time.RFC3339Nano), id)
	return err
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
