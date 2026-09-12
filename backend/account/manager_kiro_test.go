// manager_kiro_test.go 熔断/热更/错误分类测试（spec 5.2/5.6/7.x）。
package account

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestManager2 独立命名的测试构造（避免与 manager_test.go 的辅助重名冲突）。
func newBreakerManager(t *testing.T) *Manager {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "breaker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seeds := []SeedUpstream{
		{Name: "a1", Protocol: "anthropic", BaseURL: "http://x", APIKey: "k", Models: map[string]string{"m": "m"}},
		{Name: "a2", Protocol: "anthropic", BaseURL: "http://x", APIKey: "k", Models: map[string]string{"m": "m"}},
	}
	m, err := NewManager(store, seeds, nil, Cooldowns{}, ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) }
	m.probeRate = 0 // 消除试探随机性，冷却即严格跳过
	return m
}

func TestBreakerCooldownCurve(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{1, 60 * time.Second},
		{2, 120 * time.Second},
		{3, 240 * time.Second},
		{11, 17*time.Hour + 4*time.Minute}, // 60s<<10 = 61440s，未到封顶
		{12, 24 * time.Hour},               // 60s<<11 超 24h 封顶
		{50, 24 * time.Hour},               // 移位溢出防护
	}
	for _, c := range cases {
		if got := BreakerCooldown(c.n); got != c.want {
			t.Errorf("BreakerCooldown(%d) = %v, want %v", c.n, got, c.want)
		}
	}
	if BreakerCooldown(0) != 0 {
		t.Error("BreakerCooldown(0) should be 0")
	}
}

// 熔断：瞬态失败 → 指数退避冷却；冷却结束恢复调度；成功清零。
func TestBreakerTransientFailure(t *testing.T) {
	m := newBreakerManager(t)
	m.ReportTransientFailure("a1")

	// 第 1 次失败冷却 60s：当前时刻仍在冷却
	if a, ok := m.Next("m", nil); !ok || a.Name != "a2" {
		t.Fatalf("during breaker cooldown, pick = %v %v, want a2", a, ok)
	}
	// tried 排除后无号可用
	if _, ok := m.Next("m", map[string]bool{"a2": true}); ok {
		t.Fatal("a1 should be cooling (probe randomness avoided by tried)")
	}

	// 推进 61s：冷却结束
	m.now = func() time.Time { return time.Date(2026, 9, 12, 12, 1, 1, 0, time.UTC) }
	if a, ok := m.Next("m", nil); !ok || a.Name != "a1" {
		t.Fatalf("after cooldown, pick = %v %v, want a1 (sticky)", a, ok)
	}

	// 成功清零
	m.ReportSuccess("a1")
	if a := m.find("a1"); a.Failures != 0 || !a.CooldownUntil.IsZero() {
		t.Fatalf("success should reset breaker: %+v", a)
	}
}

// 熔断状态重启延续（库内恢复）。
func TestBreakerPersistAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "breaker.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seeds := []SeedUpstream{
		{Name: "a1", Protocol: "anthropic", BaseURL: "http://x", APIKey: "k", Models: map[string]string{"m": "m"}},
		{Name: "a2", Protocol: "anthropic", BaseURL: "http://x", APIKey: "k", Models: map[string]string{"m": "m"}},
	}
	m, err := NewManager(store, seeds, nil, Cooldowns{}, ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	m.ReportTransientFailure("a1")
	store.Close()

	store2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	m2, err := NewManager(store2, seeds, nil, Cooldowns{}, ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	m2.probeRate = 0
	if a, ok := m2.Next("m", nil); !ok || a.Name != "a2" {
		t.Fatalf("after restart, pick = %v %v, want a2 (a1 breaker cooling)", a, ok)
	}
	if a := m2.find("a1"); a.Failures != 1 {
		t.Fatalf("failures not persisted: %+v", a)
	}
}

// 统计记账：成功/失败累计。
func TestStatsAccounting(t *testing.T) {
	m := newBreakerManager(t)
	m.ReportSuccess("a1")
	m.ReportSuccess("a1")
	m.ReportTransientFailure("a2")
	a1, a2 := m.find("a1"), m.find("a2")
	if a1.Stats.Requests != 2 || a1.Stats.Successes != 2 || a1.Stats.Failures != 0 {
		t.Errorf("a1 stats = %+v", a1.Stats)
	}
	if a2.Stats.Requests != 1 || a2.Stats.Successes != 0 || a2.Stats.Failures != 1 {
		t.Errorf("a2 stats = %+v", a2.Stats)
	}
}

// ReportRecoverable：显式重置时刻早于熔断冷却时取较长者。
func TestReportRecoverableCooldown(t *testing.T) {
	m := newBreakerManager(t)
	// 第 1 次失败熔断冷却 60s；显式时刻 +30s 较短 → 取熔断 60s
	soon := m.now().Add(30 * time.Second)
	m.ReportRecoverable("a1", soon)
	if got := m.find("a1").CooldownUntil; !got.Equal(m.now().Add(60 * time.Second)) {
		t.Fatalf("cooldown = %v, want breaker 60s", got)
	}
	// 第 2 次失败熔断 120s；显式时刻 +1h 较长 → 取显式
	later := m.now().Add(time.Hour)
	m.ReportRecoverable("a1", later)
	if got := m.find("a1").CooldownUntil; !got.Equal(later) {
		t.Fatalf("cooldown = %v, want explicit %v", got, later)
	}
}

// Reconfigure 热更：新增账号立即可调度；更新保留运行时状态；Remove 移出。
func TestReconfigureAndRemove(t *testing.T) {
	m := newBreakerManager(t)
	m.ReportTransientFailure("a1") // a1 熔断冷却中

	// 新增账号（模拟管理 API 落库后的对象）
	m.Reconfigure(&Account{
		Name: "a3", Type: TypeAPIKey, Enabled: true,
		Protocol: "anthropic", BaseURL: "http://x", APIKey: "k",
		Models: map[string]string{"m": "m"},
	})
	if a, ok := m.Next("m", nil); !ok || a.Name != "a2" {
		t.Fatalf("after add a3, pick = %v %v, want a2 (order preserved)", a, ok)
	}

	// 更新 a2 身份（换 key）：运行时状态保留
	m.Reconfigure(&Account{
		Name: "a2", Type: TypeAPIKey, Enabled: true,
		Protocol: "anthropic", BaseURL: "http://y", APIKey: "newkey",
		Models: map[string]string{"m": "m"},
	})
	if a := m.find("a2"); a.APIKey != "newkey" {
		t.Fatalf("identity not updated: %+v", a)
	}

	// 删除 a2：a3 已 tried，a1 熔断冷却中（试探已置 0）→ 无号可调度
	m.Remove("a2")
	if _, ok := m.Next("m", map[string]bool{"a3": true}); ok {
		t.Fatal("after remove a2, no account should be schedulable")
	}
}

// 错误分类表（account_errors.py 移植）。
func TestClassifyKiroError(t *testing.T) {
	cases := []struct {
		status int
		reason string
		want   ErrorClass
	}{
		{402, "", ClassRecoverable},
		{402, ReasonMonthlyRequestCount, ClassRecoverable},
		{403, "", ClassRecoverable},
		{429, "", ClassRecoverable},
		{400, ReasonInvalidModelID, ClassRecoverable},
		{400, ReasonContentLengthExceeds, ClassFatal},
		{400, "", ClassFatal},
		{422, "", ClassFatal},
		{500, "", ClassFatal},
		{503, "", ClassFatal},
		{0, "", ClassFatal},
	}
	for _, c := range cases {
		if got := ClassifyKiroError(c.status, c.reason); got != c.want {
			t.Errorf("ClassifyKiroError(%d, %q) = %v, want %v", c.status, c.reason, got, c.want)
		}
	}
}

func TestParseKiroErrorReason(t *testing.T) {
	reason, msg := ParseKiroErrorReason([]byte(`{"message": "Input is too long.", "reason": "CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`))
	if reason != "CONTENT_LENGTH_EXCEEDS_THRESHOLD" || msg != "Input is too long." {
		t.Fatalf("reason=%q msg=%q", reason, msg)
	}
	// 字段顺序颠倒 + 转义
	reason, msg = ParseKiroErrorReason([]byte(`{"reason":"MONTHLY_REQUEST_COUNT","message":"quota \"exceeded\"\n"}`))
	if reason != "MONTHLY_REQUEST_COUNT" || msg != `quota "exceeded"`+"\n" {
		t.Fatalf("reason=%q msg=%q", reason, msg)
	}
	// 缺字段 / 非 JSON
	if reason, msg = ParseKiroErrorReason([]byte(`{"message":"no reason"}`)); reason != "" || msg != "no reason" {
		t.Fatalf("reason=%q msg=%q", reason, msg)
	}
	if reason, msg = ParseKiroErrorReason([]byte(`not json`)); reason != "" || msg != "" {
		t.Fatalf("reason=%q msg=%q", reason, msg)
	}
}
