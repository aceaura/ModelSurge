package account

import (
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"path/filepath"
	"testing"
	"time"
)

func newTestManager(t *testing.T, dbPath string) (*Manager, *Store) {
	t.Helper()
	store, err := Open(dialect.SQLite, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seeds := []*Account{
		{Name: "a1", Type: TypeAPIKey, Enabled: true, Protocol: "anthropic", BaseURL: "http://x", APIKey: "k1", Models: map[string]string{"m": "m"}},
		{Name: "a2", Type: TypeAPIKey, Enabled: true, Protocol: "anthropic", BaseURL: "http://x", APIKey: "k2", Models: map[string]string{"m": "m"}},
		{Name: "a3", Type: TypeAPIKey, Enabled: true, Protocol: "openai-chat", BaseURL: "http://y", APIKey: "k3"},
	}
	for _, a := range seeds {
		if err := store.InsertAccount(a); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewManager(store, Cooldowns{}, ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return m, store
}

// 真实 429 样例：显式重置时间戳优先于冷却时长（+1 分钟缓冲）。
func TestClassifyLimit_RealSample(t *testing.T) {
	body := `You have exceeded the 5-hour usage quota. It will reset at 2099-09-12 16:05:29 +0800 CST. We recommend upgrading your plan.`
	kind, until := classifyLimit(body, Cooldowns{}.withDefaults(), time.Now())
	if kind != "5h" {
		t.Errorf("kind = %q, want 5h", kind)
	}
	want := time.Date(2099, 9, 12, 16, 5, 29, 0, time.FixedZone("", 8*3600)).Add(resetMargin)
	if !until.Equal(want) {
		t.Errorf("until = %v, want %v", until, want)
	}
}

func TestClassifyLimit_Fallback(t *testing.T) {
	cds := Cooldowns{}.withDefaults()
	now := time.Now()

	for _, tc := range []struct {
		body string
		kind string
		dur  time.Duration
	}{
		{"exceeded the 7-hour quota", "7h", 7 * time.Hour},
		{"monthly usage limit reached", "monthly", 24 * time.Hour},
		{"本月额度已用完", "monthly", 24 * time.Hour},
		{"quota exceeded", "5h", 5 * time.Hour}, // 识别不出兜底 5h
	} {
		kind, until := classifyLimit(tc.body, cds, now)
		if kind != tc.kind {
			t.Errorf("%q: kind = %q, want %q", tc.body, kind, tc.kind)
		}
		if d := until.Sub(now); d != tc.dur {
			t.Errorf("%q: cooldown = %v, want %v", tc.body, d, tc.dur)
		}
	}
}

// 单账号特例：无视限流/熔断冷却恒返回（account_manager.py 语义）；
// tried 排除与显式禁用仍生效。
func TestSingleAccountBypassCooldown(t *testing.T) {
	store, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "single.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertAccount(&Account{
		Name: "solo", Type: TypeAPIKey, Enabled: true, Protocol: "anthropic", BaseURL: "http://x", APIKey: "k1",
		Models: map[string]string{"m": "m"},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(store, Cooldowns{}, ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}

	// 限流冷却中：仍返回
	m.ReportLimit("solo", "monthly usage limit reached")
	if a, ok := m.Next("m", nil); !ok || a.Name != "solo" {
		t.Fatalf("limit-cooled single account must still be returned, got %v %v", a, ok)
	}

	// 熔断冷却中：仍返回
	m.ReportTransientFailure("solo")
	if a, ok := m.Next("m", nil); !ok || a.Name != "solo" {
		t.Fatalf("breaker-cooled single account must still be returned, got %v %v", a, ok)
	}

	// tried 排除：不返回
	if _, ok := m.Next("m", map[string]bool{"solo": true}); ok {
		t.Error("tried exclusion must still apply in single-account bypass")
	}

	// 显式禁用：不返回
	m.ReportAuthFailure("solo")
	if _, ok := m.Next("m", nil); ok {
		t.Error("disabled single account must not be returned")
	}
}

// 粘性：恒选序号最小的可用账号；冷却/禁用跳过；冷却结束自动切回。
func TestStickyPick(t *testing.T) {
	m, _ := newTestManager(t, filepath.Join(t.TempDir(), "sticky.db"))
	now := time.Now()

	a, ok := m.Next("m", nil)
	if !ok || a.Name != "a1" {
		t.Fatalf("first pick = %v %v, want a1", a, ok)
	}

	m.ReportLimit("a1", "exceeded the 5-hour usage quota")
	a, ok = m.Next("m", nil)
	if !ok || a.Name != "a2" {
		t.Fatalf("after a1 cooling, pick = %v %v, want a2", a, ok)
	}

	// tried 跳过 a2 -> a3（透传型无 models 也可服务）
	a, _ = m.Next("m", map[string]bool{"a2": true})
	if a.Name != "a3" {
		t.Fatalf("with a2 tried, pick = %s, want a3", a.Name)
	}

	// 时钟快进到冷却结束后：切回 a1（缓存最热的账号）
	m.now = func() time.Time { return now.Add(6 * time.Hour) }
	a, _ = m.Next("m", nil)
	if a.Name != "a1" {
		t.Fatalf("after cooldown, pick = %s, want a1 back", a.Name)
	}
}

// 401/403：key 失效，账号禁用不再被选中。
func TestAuthFailureDisables(t *testing.T) {
	m, _ := newTestManager(t, filepath.Join(t.TempDir(), "auth.db"))
	m.ReportAuthFailure("a1")
	a, ok := m.Next("m", nil)
	if !ok || a.Name != "a2" {
		t.Fatalf("after a1 disabled, pick = %v %v, want a2", a, ok)
	}
	m.ReportAuthFailure("a2")
	a, ok = m.Next("m", nil)
	if !ok || a.Name != "a3" {
		t.Fatalf("after a2 disabled, pick = %v %v, want a3", a, ok)
	}
}

// usage 逐笔记账 + 窗口累计；重启后冷却状态与历史用量都在。
func TestUsageAccountingAndRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	m, store := newTestManager(t, dbPath)

	m.ReportUsage("a1", Usage{InputTokens: 100, OutputTokens: 50, CacheRead: 200})
	m.ReportUsage("a1", Usage{InputTokens: 300, OutputTokens: 20})
	u, err := m.WindowUsage("a1", time.Now().Add(-5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 400 || u.OutputTokens != 70 || u.CacheRead != 200 {
		t.Errorf("window usage = %+v", u)
	}
	if u.Total() != 670 {
		t.Errorf("total = %d, want 670", u.Total())
	}

	m.ReportLimit("a1", "exceeded the 5-hour usage quota")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：重开同一 DB，冷却状态必须延续
	store2, err := Open(dialect.SQLite, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	m2, err := NewManager(store2, Cooldowns{}, ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := m2.Next("m", nil)
	if a.Name != "a2" {
		t.Fatalf("after restart, pick = %s, want a2 (a1 still cooling)", a.Name)
	}
	u, _ = m2.WindowUsage("a1", time.Now().Add(-5*time.Hour))
	if u.InputTokens != 400 {
		t.Errorf("usage after restart = %+v, history lost", u)
	}
}

// 库内账号读取 + 管理面建号并存：NewManager 从库装载，
// 建号后经 Reconfigure 进入调度。
func TestNewManagerFromStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync.db")
	m, store := newTestManager(t, dbPath)
	defer store.Close()

	// 管理面创建 kiro 账号
	if err := store.InsertAccount(&Account{
		Name: "api-created", Type: TypeKiro, Enabled: true,
		Kiro: &KiroAccount{Source: "refresh_token", RefreshToken: "rt"},
	}); err != nil {
		t.Fatal(err)
	}
	accs, err := store.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 4 {
		t.Fatalf("accounts = %d, want 4 (a1..a3 + api-created)", len(accs))
	}
	if accs[3].Name != "api-created" || accs[3].Type != TypeKiro {
		t.Errorf("api-created = %s %q, want kiro", accs[3].Name, accs[3].Type)
	}

	// 热加载进调度（管理 API 路径），可被选中服务
	m.Reconfigure(&accs[3])
	if a, ok := m.Next("m", map[string]bool{"a1": true, "a2": true, "a3": true}); !ok || a.Name != "api-created" {
		t.Fatalf("pick = %v %v, want api-created", a, ok)
	}
}

// 换 key 热更解除 401 自动禁用（并落库）；key 未变时禁用保留。
func TestReconfigureCredentialChangeReenables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reenable.db")
	m, store := newTestManager(t, dbPath)

	m.ReportAuthFailure("a1")
	if _, ok := m.Next("m", map[string]bool{"a2": true, "a3": true}); ok {
		t.Fatal("disabled a1 must not serve")
	}

	// key 未变：禁用保留
	m.Reconfigure(&Account{Name: "a1", Type: TypeAPIKey, Enabled: true, Protocol: "anthropic", BaseURL: "http://x", APIKey: "k1", Models: map[string]string{"m": "m"}})
	if _, ok := m.Next("m", map[string]bool{"a2": true, "a3": true}); ok {
		t.Fatal("same key must keep a1 disabled")
	}

	// 换 key：解除禁用并恢复调度
	m.Reconfigure(&Account{Name: "a1", Type: TypeAPIKey, Enabled: true, Protocol: "anthropic", BaseURL: "http://x", APIKey: "k1-new", Models: map[string]string{"m": "m"}})
	if a, ok := m.Next("m", map[string]bool{"a2": true, "a3": true}); !ok || a.Name != "a1" {
		t.Fatalf("pick = %v %v, want a1 re-enabled", a, ok)
	}
	accs, err := store.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accs {
		if a.Name == "a1" && a.Disabled {
			t.Error("a1 disabled flag must be cleared in store")
		}
	}
}
