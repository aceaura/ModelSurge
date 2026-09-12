package account

import (
	"log"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Cooldowns 各级限流窗口触发后的冷却时长。
type Cooldowns struct {
	Default  time.Duration // 无法识别窗口时（默认 5h）
	Window7h time.Duration // 7 小时窗口（默认 7h）
	Monthly  time.Duration // 月度窗口（默认 24h，次日再试；月度真实重置时间未知）
}

func (c Cooldowns) withDefaults() Cooldowns {
	if c.Default <= 0 {
		c.Default = 5 * time.Hour
	}
	if c.Window7h <= 0 {
		c.Window7h = 7 * time.Hour
	}
	if c.Monthly <= 0 {
		c.Monthly = 24 * time.Hour
	}
	return c
}

// 熔断参数（spec 5.2）：连续失败指数退避 60s×2^(n-1) 封顶 24h；
// 冷却中 10% 概率试探（Half-Open）；成功清零。
// 基数/封顶/概率可经 ManagerDeps 覆盖（config kiro 段）。
const (
	breakerBaseCooldown = 60 * time.Second
	breakerMaxCooldown  = 24 * time.Hour
	breakerProbeRate    = 0.1
)

// BreakerCooldown 第 n 次连续失败的冷却时长（默认参数：60s×2^(n-1)，封顶 24h）。
func BreakerCooldown(n int) time.Duration {
	return breakerCooldown(breakerBaseCooldown, breakerMaxCooldown, n)
}

// breakerCooldown 指定基数与封顶的退避时长。
func breakerCooldown(base, max time.Duration, n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := base << (n - 1)
	if d > max || d <= 0 { // 移位溢出防护
		return max
	}
	return d
}

// ManagerDeps 调度器可选依赖（零值全默认；config 的 kiro 段装配）。
type ManagerDeps struct {
	BreakerBase  time.Duration // 熔断指数退避基数；0 = 60s
	BreakerMax   time.Duration // 熔断冷却封顶；0 = 24h
	ProbeRate    float64       // 熔断半开试探概率；0 = 0.1
	KiroRegion   string        // kiro 账号 SSO 区默认（账号级 region 可覆盖）
	KiroCacheTTL time.Duration // kiro 模型缓存 TTL；0 = 12h
}

// Manager 账号调度状态机。
// 粘性策略：恒选序号最小的可用账号（未禁用且不在冷却），
// 冷却结束自动切回原账号——保证 prompt cache 命中率，绝不轮换。
type Manager struct {
	mu           sync.Mutex
	store        *Store
	order        []*Account // 按配置序，调度即按序取第一个可用者
	cooldowns    Cooldowns
	kiro         map[string]*KiroRuntime // kiro 账号运行时（懒初始化）
	probeRate    float64                 // 熔断冷却试探概率（Half-Open），测试可注入 0
	breakerBase  time.Duration           // 熔断指数退避基数
	breakerMax   time.Duration           // 熔断冷却封顶
	kiroRegion   string                  // kiro SSO 区默认（账号级可覆盖）
	kiroCacheTTL time.Duration           // kiro 模型缓存 TTL
	now          func() time.Time        // 测试可注入时钟
}

// NewManager 构造调度器：账号身份从 seeds 同步（api-key 与 kiro 种子均
// upsert-only，以 yaml 为准），状态（冷却/禁用）沿用库内值；
// kiro 账号构造运行时（无网络操作）。deps 零值走默认参数。
func NewManager(store *Store, seeds []SeedUpstream, kiroSeeds []SeedKiro, cds Cooldowns, deps ManagerDeps) (*Manager, error) {
	if err := store.SyncAccounts(seeds); err != nil {
		return nil, err
	}
	if err := store.SyncKiroAccounts(kiroSeeds); err != nil {
		return nil, err
	}
	accs, err := store.ListAccounts()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		store: store, cooldowns: cds.withDefaults(), kiro: map[string]*KiroRuntime{},
		probeRate: breakerProbeRate, breakerBase: breakerBaseCooldown, breakerMax: breakerMaxCooldown,
		kiroCacheTTL: DefaultModelCacheTTL, now: time.Now,
	}
	if deps.BreakerBase > 0 {
		m.breakerBase = deps.BreakerBase
	}
	if deps.BreakerMax > 0 {
		m.breakerMax = deps.BreakerMax
	}
	if deps.ProbeRate > 0 {
		m.probeRate = deps.ProbeRate
	}
	m.kiroRegion = deps.KiroRegion
	if deps.KiroCacheTTL > 0 {
		m.kiroCacheTTL = deps.KiroCacheTTL
	}
	for i := range accs {
		m.order = append(m.order, &accs[i])
		if accs[i].Type == TypeKiro {
			m.ensureKiroRuntime(&accs[i])
		}
	}
	return m, nil
}

// Next 返回能服务 model 的第一个可用账号（跳过 tried 中已试过的）。
// 粘性来源：只要状态不变，Next 的结果不变。
// 熔断冷却中的账号以 10% 概率试探放行（Half-Open）。
func (m *Manager) Next(model string, tried map[string]bool) (*Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.order {
		if tried[a.Name] || !a.Enabled || a.Disabled {
			continue
		}
		if a.CooldownUntil.After(m.now()) {
			// 限流冷却（Failures==0）：严格跳过；熔断冷却：低概率试探（Half-Open）
			if a.Failures == 0 || rand.Float64() >= m.probeRate {
				continue
			}
		}
		if _, ok := a.Serving(model); !ok {
			continue
		}
		if a.Type == TypeKiro {
			if rt := m.kiro[a.Name]; rt != nil {
				rt.warmModelsAsync() // 非阻塞：缓存过期才后台拉取
			}
		}
		return a, true
	}
	return nil, false
}

// ReportLimit 上报限流（429）：识别限流窗口并让账号进入冷却。
// body 为上游错误体文本。错误体带重置时间戳时直接冷却到该时刻
// （+1 分钟缓冲，如 "It will reset at 2026-09-12 16:05:29 +0800 CST"）；
// 否则按窗口关键词匹配冷却时长（"5-hour"/"7-hour"/"monthly"），兜底 5h。
func (m *Manager) ReportLimit(name, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	kind, until := classifyLimit(body, m.cooldowns, now)
	for _, a := range m.order {
		if a.Name == name {
			a.LimitKind, a.CooldownUntil = kind, until
			break
		}
	}
	if err := m.store.SetCooldown(name, kind, until); err != nil {
		log.Printf("account: set cooldown %s: %v", name, err)
	}
	w5, _ := m.store.WindowUsage(name, now.Add(-5*time.Hour))
	w7, _ := m.store.WindowUsage(name, now.Add(-7*time.Hour))
	log.Printf("account: %s hit %s limit, cooling down until %s (usage 5h=%d 7h=%d tokens)",
		name, kind, until.Format("15:04:05"), w5.Total(), w7.Total())
}

// ReportAuthFailure 上报鉴权失败（401/403）：key 失效，禁用账号待人工换 key。
func (m *Manager) ReportAuthFailure(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.order {
		if a.Name == name {
			a.Disabled = true
			break
		}
	}
	if err := m.store.SetDisabled(name, true); err != nil {
		log.Printf("account: disable %s: %v", name, err)
	}
	log.Printf("account: %s auth failed (bad key?), disabled; fix key in yaml and restart", name)
}

// ReportUsage 记一笔真实用量并落库。
func (m *Manager) ReportUsage(name string, u Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.InsertUsage(name, m.now(), u); err != nil {
		log.Printf("account: insert usage %s: %v", name, err)
	}
}

// ReportSuccess 上报一次成功：熔断清零（冷却一并解除）+ 统计记账。
func (m *Manager) ReportSuccess(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.find(name)
	if a == nil {
		return
	}
	hadBreaker := a.Failures > 0
	a.Failures = 0
	a.CooldownUntil = time.Time{}
	a.LimitKind = ""
	a.Stats.Requests++
	a.Stats.Successes++
	a.Stats.LastUsedAt = m.now().Unix()
	m.persistStats(a)
	if hadBreaker {
		if err := m.store.SetFailures(name, 0, time.Time{}); err != nil {
			log.Printf("account: reset failures %s: %v", name, err)
		}
		log.Printf("account: %s recovered, breaker reset", name)
	}
}

// ReportTransientFailure 上报一次瞬态失败（网络/5xx/首事件超时重试耗尽）：
// 熔断计数 +1 并按指数退避冷却。
func (m *Manager) ReportTransientFailure(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.find(name)
	if a == nil {
		return
	}
	a.Failures++
	a.LastFailure = m.now()
	a.CooldownUntil = m.now().Add(m.breakerCooldown(a.Failures))
	a.Stats.Requests++
	a.Stats.Failures++
	a.Stats.LastUsedAt = m.now().Unix()
	m.persistStats(a)
	if err := m.store.SetFailures(name, a.Failures, a.LastFailure); err != nil {
		log.Printf("account: set failures %s: %v", name, err)
	}
	if err := m.store.SetCooldown(name, a.LimitKind, a.CooldownUntil); err != nil {
		log.Printf("account: set cooldown %s: %v", name, err)
	}
	log.Printf("account: %s transient failure #%d, cooling down until %s",
		name, a.Failures, a.CooldownUntil.Format("15:04:05"))
}

// ReportRecoverable 上报一次可恢复错误（402 配额等）并冷却到显式时刻；
// 时刻早于当前熔断冷却则取较长者（显式重置时间优先，但熔断退避不放松）。
func (m *Manager) ReportRecoverable(name string, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.find(name)
	if a == nil {
		return
	}
	a.Failures++
	a.LastFailure = m.now()
	breakerUntil := m.now().Add(m.breakerCooldown(a.Failures))
	if until.After(breakerUntil) {
		a.CooldownUntil = until
	} else {
		a.CooldownUntil = breakerUntil
	}
	a.Stats.Requests++
	a.Stats.Failures++
	a.Stats.LastUsedAt = m.now().Unix()
	m.persistStats(a)
	if err := m.store.SetFailures(name, a.Failures, a.LastFailure); err != nil {
		log.Printf("account: set failures %s: %v", name, err)
	}
	if err := m.store.SetCooldown(name, a.LimitKind, a.CooldownUntil); err != nil {
		log.Printf("account: set cooldown %s: %v", name, err)
	}
	log.Printf("account: %s recoverable failure #%d, cooling down until %s",
		name, a.Failures, a.CooldownUntil.Format("15:04:05"))
}

// find 按名取账号指针（须持锁）。
func (m *Manager) find(name string) *Account {
	for _, a := range m.order {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// breakerCooldown 本调度器参数下的退避时长（基数×2^(n-1)，封顶 max）。
func (m *Manager) breakerCooldown(n int) time.Duration {
	return breakerCooldown(m.breakerBase, m.breakerMax, n)
}

// persistStats 统计落库（须持锁；失败仅记日志，内存值仍生效）。
func (m *Manager) persistStats(a *Account) {
	if err := m.store.SaveStats(a.Name, a.Stats); err != nil {
		log.Printf("account: save stats %s: %v", a.Name, err)
	}
}

// Reconfigure 热更/新增账号（管理 API 落库后调用）。
// 已在调度中的账号就地替换身份字段（保留运行时状态）；
// 新账号追加到队尾；kiro 账号重建运行时（凭据可能已变）。
func (m *Manager) Reconfigure(a *Account) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.find(a.Name); existing != nil {
		updated := *a
		updated.Disabled = existing.Disabled
		updated.LimitKind = existing.LimitKind
		updated.CooldownUntil = existing.CooldownUntil
		updated.Failures = existing.Failures
		updated.LastFailure = existing.LastFailure
		updated.Stats = existing.Stats
		updated.UpdatedAt = existing.UpdatedAt
		*existing = updated
		*a = updated // 调用方对象同步为生效态
		if existing.Type == TypeKiro {
			m.ensureKiroRuntime(existing)
		}
		return
	}
	m.order = append(m.order, a)
	if a.Type == TypeKiro {
		m.ensureKiroRuntime(a)
	}
}

// Remove 把账号移出调度（管理 API 删除后调用；usage 历史保留在库）。
func (m *Manager) Remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, a := range m.order {
		if a.Name == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			delete(m.kiro, name)
			return
		}
	}
}

// Store 暴露底层存储（管理面 CRUD 落库用）。
func (m *Manager) Store() *Store { return m.store }

// KiroRuntimeOf 取 kiro 账号的运行时（relay 端点解析用；未初始化返回 nil）。
func (m *Manager) KiroRuntimeOf(name string) *KiroRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kiro[name]
}

// WindowUsage 查询账号自 since 起的用量合计（监控口径）。
func (m *Manager) WindowUsage(name string, since time.Time) (Usage, error) {
	return m.store.WindowUsage(name, since)
}

// Status 全部账号的调度快照（日志/调试用）。
func (m *Manager) Status() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Account, len(m.order))
	for i, a := range m.order {
		out[i] = *a
	}
	return out
}

// Total 折算总 token（监控口径：input + output + cache 读写全计）。
func (u Usage) Total() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheRead + u.CacheCreation
}

// resetMargin 重置时刻后的缓冲：窗口边界瞬间恢复可能不完整。
const resetMargin = time.Minute

// resetAtRe 匹配限流报错中的重置时间戳，如
// "It will reset at 2026-09-12 16:05:29 +0800 CST"。
var resetAtRe = regexp.MustCompile(`reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) ([+-]\d{4})`)

// classifyLimit 从 429 错误体判定限流窗口与冷却截止时刻。
// 优先级：显式重置时间戳 > 窗口关键词 > 兜底 5h（最短窗口，
// 冷却后重试探测，误判代价小）。错误体为中英双语关键词匹配。
func classifyLimit(body string, cds Cooldowns, now time.Time) (kind string, until time.Time) {
	if m := resetAtRe.FindStringSubmatch(body); m != nil {
		if t, err := time.Parse("2006-01-02 15:04:05 -0700", m[1]+" "+m[2]); err == nil && t.After(now) {
			return classifyWindow(body), t.Add(resetMargin)
		}
	}
	return classifyWindow(body), now.Add(cooldownDur(body, cds))
}

func classifyWindow(body string) string {
	switch {
	case strings.Contains(body, "7-hour") || strings.Contains(body, "7 hour") ||
		strings.Contains(body, "7小时") || strings.Contains(body, "7 小时"):
		return "7h"
	case strings.Contains(strings.ToLower(body), "month") || strings.Contains(body, "月"):
		return "monthly"
	default:
		return "5h"
	}
}

func cooldownDur(body string, cds Cooldowns) time.Duration {
	switch classifyWindow(body) {
	case "7h":
		return cds.Window7h
	case "monthly":
		return cds.Monthly
	default:
		return cds.Default
	}
}
