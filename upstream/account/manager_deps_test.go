// manager_deps_test.go Manager 从库装载与 ManagerDeps（config kiro 段装配）测试。
package account

import (
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"path/filepath"
	"testing"
	"time"
)

// mustInsert 依次落库账号（测试辅助）。
func mustInsert(t *testing.T, store *Store, accs ...*Account) {
	t.Helper()
	for _, a := range accs {
		if err := store.InsertAccount(a); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNewManagerLoadsKiroAccounts NewManager 装载库内 kiro 账号并构造运行时；
// api-key 账号不受影响。
func TestNewManagerLoadsKiroAccounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kiroseed.db")
	store, err := Open(dialect.SQLite, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	mustInsert(t, store,
		&Account{Name: "ark-1", Type: TypeAPIKey, Enabled: true, Protocol: "anthropic", BaseURL: "http://x", APIKey: "k", Models: map[string]string{"m": "m"}},
		&Account{Name: "k1", Type: TypeKiro, Enabled: true, Kiro: &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt-v1", Region: "us-east-1", WebSearch: true}},
	)
	if _, err := NewManager(store, Cooldowns{}, ManagerDeps{}); err != nil {
		t.Fatal(err)
	}

	acc, err := store.GetAccount("k1")
	if err != nil || acc == nil {
		t.Fatalf("k1 not loaded: %v %v", acc, err)
	}
	if acc.Type != TypeKiro || acc.Kiro == nil || acc.Kiro.RefreshToken != "rt-v1" || !acc.Kiro.WebSearch {
		t.Fatalf("k1 identity: %+v", acc)
	}
	if acc, _ := store.GetAccount("ark-1"); acc == nil {
		t.Fatal("api-key account should be untouched")
	}
}

// TestManagerDepsBreaker 熔断参数覆盖：基数 10s、封顶 30s。
func TestManagerDepsBreaker(t *testing.T) {
	store, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "deps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mustInsert(t, store,
		&Account{Name: "a1", Type: TypeAPIKey, Enabled: true, Protocol: "anthropic", BaseURL: "http://x", APIKey: "k", Models: map[string]string{"m": "m"}},
	)
	m, err := NewManager(store, Cooldowns{}, ManagerDeps{
		BreakerBase: 10 * time.Second,
		BreakerMax:  30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) }
	m.probeRate = 0
	if got := m.breakerCooldown(1); got != 10*time.Second {
		t.Fatalf("breakerCooldown(1) = %v, want 10s", got)
	}
	if got := m.breakerCooldown(2); got != 20*time.Second {
		t.Fatalf("breakerCooldown(2) = %v, want 20s", got)
	}
	if got := m.breakerCooldown(3); got != 30*time.Second {
		t.Fatalf("breakerCooldown(3) = %v, want 30s (cap)", got)
	}
}

// TestManagerDepsKiroDefaults kiro 区默认与缓存 TTL 注入。
func TestManagerDepsKiroDefaults(t *testing.T) {
	store, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "kdeps.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mustInsert(t, store,
		&Account{Name: "k1", Type: TypeKiro, Enabled: true, Kiro: &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt"}}, // Region 留空
		&Account{Name: "k2", Type: TypeKiro, Enabled: true, Kiro: &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt", Region: "ap-southeast-1"}},
	)
	m, err := NewManager(store, Cooldowns{}, ManagerDeps{
		KiroRegion:   "eu-central-1",
		KiroCacheTTL: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 全局默认填充：账号未配 region 的用全局；账号显式 region 优先
	if got := m.kiro["k1"].Auth.SSORegion(); got != "eu-central-1" {
		t.Fatalf("k1 SSORegion = %q, want eu-central-1", got)
	}
	if got := m.kiro["k2"].Auth.SSORegion(); got != "ap-southeast-1" {
		t.Fatalf("k2 SSORegion = %q, want ap-southeast-1", got)
	}
	// 账号本体不被改写（region 只作用于运行时）
	if acc, _ := store.GetAccount("k1"); acc.Kiro.Region != "" {
		t.Fatalf("account region should stay empty, got %q", acc.Kiro.Region)
	}
	// 缓存 TTL 注入
	if got := m.kiro["k1"].Models.Stale(); !got {
		t.Fatal("cache never updated should be stale")
	}
	m.kiro["k1"].Models.Update(nil)
	if got := m.kiro["k1"].Models.Stale(); got {
		t.Fatal("just-updated cache should be fresh")
	}
	m.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	m.kiro["k1"].Models.now = m.now
	if got := m.kiro["k1"].Models.Stale(); !got {
		t.Fatal("cache should be stale after custom TTL")
	}
}
