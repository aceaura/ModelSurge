// store_lock.go 跨副本账号级互斥与 token 状态重读（postgres 集群形态）。
// sqlite 单进程由进程内互斥（AuthService.mu）兜底，LockAccount 为 no-op。
package account

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"time"

	"github.com/aceaura/ModelSurge/upstream/dialect"
)

// unlockTimeout 解锁/归还连接的防御上限：advisory unlock 本不阻塞，
// 卡死也不拖垮持锁方。
const unlockTimeout = 10 * time.Second

// AccountLock 账号级跨副本互斥句柄；Unlock 幂等（nil / 空句柄为 no-op）。
// postgres 实现：pinned 连接上的 session 级 pg_advisory_lock——刷新含
// 跨 HTTP 长调用，不能持事务等网络（xact 级锁不适用）；进程崩溃时
// 连接断开，锁由服务端自动释放。
type AccountLock struct {
	conn *sql.Conn
	key  int64
}

// Unlock 释放 advisory lock 并归还连接；重复调用安全。
// 直连 pinned conn，不经 s.q（该路径仅 postgres 触达，$1 原生写）。
func (l *AccountLock) Unlock() {
	if l == nil || l.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	_, _ = l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, l.key)
	l.conn.Close()
	l.conn = nil
}

// LockAccount 获取账号级跨副本互斥：postgres 下取池内 pinned 连接并对
// 账号名 hash 加 session advisory lock（阻塞直到他副本释放或 ctx 取消）；
// sqlite 单进程无竞争，返回 no-op 句柄。
func (s *Store) LockAccount(ctx context.Context, name string) (*AccountLock, error) {
	if s.driver != dialect.Postgres {
		return &AccountLock{}, nil
	}
	sum := sha256.Sum256([]byte("modelsurge-account:" + name))
	key := int64(binary.BigEndian.Uint64(sum[:8]))
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Close()
		return nil, err
	}
	return &AccountLock{conn: conn, key: key}, nil
}

// GetTokenState 读取账号已持久化的 token 状态（锁内重读采纳用；
// 账号不存在/列为空/解析失败返回 nil）。
func (s *Store) GetTokenState(name string) (*TokenState, error) {
	var b string
	err := s.db.QueryRow(s.q(`SELECT COALESCE(token_state,'') FROM accounts WHERE name=?`), name).Scan(&b)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b == "" {
		return nil, nil
	}
	var ts TokenState
	if json.Unmarshal([]byte(b), &ts) != nil {
		return nil, nil
	}
	return &ts, nil
}
