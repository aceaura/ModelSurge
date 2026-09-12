package account

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestManager(t *testing.T, dbPath string) (*Manager, *Store) {
	t.Helper()
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seeds := []SeedUpstream{
		{Name: "a1", Protocol: "anthropic", BaseURL: "http://x", APIKey: "k1", Models: map[string]string{"m": "m"}},
		{Name: "a2", Protocol: "anthropic", BaseURL: "http://x", APIKey: "k2", Models: map[string]string{"m": "m"}},
		{Name: "a3", Protocol: "openai-chat", BaseURL: "http://y", APIKey: "k3"},
	}
	m, err := NewManager(store, seeds, nil, Cooldowns{}, ManagerDeps{})
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
	m2, store2 := newTestManager(t, dbPath)
	defer store2.Close()
	a, _ := m2.Next("m", nil)
	if a.Name != "a2" {
		t.Fatalf("after restart, pick = %s, want a2 (a1 still cooling)", a.Name)
	}
	u, _ = m2.WindowUsage("a1", time.Now().Add(-5*time.Hour))
	if u.InputTokens != 400 {
		t.Errorf("usage after restart = %+v, history lost", u)
	}
}

// yaml 种子同步：加账号、改 key 以 yaml 为准；upsert-only，
// yaml 中已移除的账号不删除（管理 API 创建的账号与 yaml 并存）。
func TestSyncAccounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync.db")
	m, store := newTestManager(t, dbPath)
	defer store.Close()

	// 模拟管理 API 创建的账号
	if err := store.InsertAccount(&Account{
		Name: "api-created", Type: TypeKiro, Enabled: true,
		Kiro: &KiroAccount{Source: "refresh_token", RefreshToken: "rt"},
	}); err != nil {
		t.Fatal(err)
	}

	seeds := []SeedUpstream{
		{Name: "a1", Protocol: "anthropic", BaseURL: "http://x", APIKey: "k1-new", Models: map[string]string{"m": "m"}},
		{Name: "a4", Protocol: "gemini", BaseURL: "http://z", APIKey: "k4", Models: map[string]string{"m": "m"}},
	}
	if err := store.SyncAccounts(seeds); err != nil {
		t.Fatal(err)
	}
	accs, err := store.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 5 {
		t.Fatalf("accounts = %d, want 5 (a1..a3 + a4 + api-created)", len(accs))
	}
	if accs[0].APIKey != "k1-new" {
		t.Errorf("a1 key = %q, want updated k1-new", accs[0].APIKey)
	}
	if accs[3].Name != "api-created" || accs[3].Type != TypeKiro {
		t.Errorf("api-created = %s %q, want kiro (must survive seed sync)", accs[3].Name, accs[3].Type)
	}
	_ = m
}
