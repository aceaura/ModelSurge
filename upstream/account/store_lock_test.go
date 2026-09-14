package account

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/dialect/pgtest"
)

// sqlite 单进程：LockAccount 为 no-op 句柄，Unlock 幂等。
func TestLockAccount_SqliteNoop(t *testing.T) {
	store := newAuthStore(t)
	l, err := store.LockAccount(context.Background(), "any")
	if err != nil || l == nil {
		t.Fatalf("LockAccount = %v, %v", l, err)
	}
	l.Unlock()
	l.Unlock() // 重复解锁安全

	var nilLock *AccountLock
	nilLock.Unlock() // nil 句柄安全
}

// 并发取锁：sqlite 全部立即成功（进程内互斥由上层 mu 保证）。
func TestLockAccount_SqliteConcurrent(t *testing.T) {
	store := newAuthStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := store.LockAccount(context.Background(), "same-account")
			if err != nil {
				t.Error(err)
				return
			}
			time.Sleep(time.Millisecond)
			l.Unlock()
		}()
	}
	wg.Wait()
}

// GetTokenState：缺账号/空列/正常路径。
func TestGetTokenState(t *testing.T) {
	store := newAuthStore(t)
	if ts, err := store.GetTokenState("missing"); err != nil || ts != nil {
		t.Fatalf("missing account: %v, %v", ts, err)
	}
	if err := store.InsertAccount(&Account{
		Name: "k", Type: TypeKiro, Enabled: true,
		Kiro: &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt"},
	}); err != nil {
		t.Fatal(err)
	}
	if ts, err := store.GetTokenState("k"); err != nil || ts != nil {
		t.Fatalf("empty column: %v, %v", ts, err)
	}
	fresh := time.Now().Add(2 * time.Hour)
	if err := store.SaveTokenState("k", &TokenState{
		AccessToken: "at-new", RefreshToken: "rt-rotated", ExpiresAt: fresh,
	}); err != nil {
		t.Fatal(err)
	}
	ts, err := store.GetTokenState("k")
	if err != nil || ts == nil {
		t.Fatalf("saved state: %v, %v", ts, err)
	}
	if ts.AccessToken != "at-new" || ts.RefreshToken != "rt-rotated" || !ts.ExpiresAt.Equal(fresh) {
		t.Errorf("token state = %+v", ts)
	}
}

// 锁内采纳：库内 token 比内存新且未临期 → 整份采纳；
// 更旧/临期/缺失则不采纳（照常走刷新）。
func TestAdoptPersistedToken(t *testing.T) {
	store := newAuthStore(t)
	if err := store.InsertAccount(&Account{
		Name: "k", Type: TypeKiro, Enabled: true,
		Kiro: &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt-old"},
	}); err != nil {
		t.Fatal(err)
	}
	s := NewAuthService(store, "k", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt-old"})
	s.token = TokenState{
		AccessToken: "at-fresh", RefreshToken: "rt-old",
		ExpiresAt: time.Now().Add(time.Hour),
	}

	// 库内更旧：不采纳
	if err := store.SaveTokenState("k", &TokenState{AccessToken: "at-older", ExpiresAt: time.Now().Add(30 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if s.adoptPersistedToken() {
		t.Fatal("adopted a staler persisted token")
	}

	// 库内更新且未临期：采纳
	s.token.ExpiresAt = time.Now().Add(time.Minute) // 内存临期触发刷新路径
	if err := store.SaveTokenState("k", &TokenState{
		AccessToken: "at-new", RefreshToken: "rt-rotated", ExpiresAt: time.Now().Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if !s.adoptPersistedToken() {
		t.Fatal("did not adopt fresher persisted token")
	}
	if s.token.AccessToken != "at-new" || s.token.RefreshToken != "rt-rotated" {
		t.Errorf("adopted state = %+v", s.token)
	}

	// 二次采纳（不再更旧）：不重复采纳
	if s.adoptPersistedToken() {
		t.Fatal("re-adopted same persisted token")
	}
}

// 无 store（纯内存）：采纳与加锁均为 no-op。
func TestAdoptPersistedToken_NoStore(t *testing.T) {
	s := NewAuthService(nil, "", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt"})
	if s.adoptPersistedToken() {
		t.Fatal("adopted without store")
	}
	if _, err := s.lockAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// testPGDSN 跨方言门控：TEST_PG_DSN 未设时跳过该测试。
// account 与 upstreamstore 同属 upstream schema 域，共用 <base>_upstream 库。
func testPGDSN(t *testing.T) string {
	t.Helper()
	return pgtest.ModuleDSN(t, "upstream")
}

// postgres 门控（TEST_PG_DSN）：advisory lock 互斥 + 锁内采纳语义。
func TestLockAccount_Postgres(t *testing.T) {
	store, err := Open(dialect.Postgres, testPGDSN(t))
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	name := "pg-lock-" + filepath.Base(t.TempDir())
	l1, err := store.LockAccount(ctx, name)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer l1.Unlock()

	// 他持锁时二次加锁阻塞：短超时 ctx 应失败
	ctx2, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := store.LockAccount(ctx2, name); err == nil {
		t.Fatal("second lock unexpectedly acquired while held")
	}

	// 不同账号不互斥
	l2, err := store.LockAccount(ctx, name+"-other")
	if err != nil {
		t.Fatalf("lock other account: %v", err)
	}
	l2.Unlock()

	l1.Unlock()
	l3, err := store.LockAccount(ctx, name)
	if err != nil {
		t.Fatalf("re-lock after unlock: %v", err)
	}
	l3.Unlock()
}
