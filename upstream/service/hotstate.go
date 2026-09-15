// hotstate.go Redis 热态（design/deployment-modes.md 2.4）：
// ① Evaluate 读旁路——按 id 缓存可用性态（enabled/cooldown/failures/class），
//   miss 批量回源一次 ListModels 并回填；报告写路径主动失效（TTL 60s 兜底）。
// ② Half-Open 试探——熔断退避类冷却经 Redis SET NX PX 收敛为全局单试探
//   （多副本下 rand 概率会放大 N 倍）；Redis 故障回退 rand（设计 2.5），
//   未配置（模式一）= 现行为严格跳过。
// DB 恒权威：缓存/锁全易失，任何 Redis 错误都降级为直读 DB。
package upstream

import (
	"context"
	"encoding/json"
	"math/rand"
	"time"

	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

const (
	stateCacheTTL = 60 * time.Second
	probeLockTTL  = 60 * time.Second
	probeRate     = 0.1
)

// stateCacheEntry Evaluate 读旁路的缓存条目（PG model_state + enabled 的投影）。
type stateCacheEntry struct {
	Found    bool   `json:"f"`
	Enabled  bool   `json:"e"`
	Cooldown int64  `json:"c"` // unix 秒；0 = 无冷却
	Failures int    `json:"n"`
	Class    string `json:"k"`
	Account  string `json:"a"`
	// Window 上下文窗口（upstream_models.context_window；0=未知）；
	// Native 原生模型名——两者供 Evaluate 的窗口回填。旧缓存条目缺字段
	// 反序列化为 0/""（不过滤，60s TTL 自然收敛）。
	Window  int    `json:"w,omitempty"`
	Native  string `json:"m,omitempty"`
}

func stateKey(id string) string { return "state:" + id }

// probeEligible 熔断退避类冷却才试探：鉴权/限流/显式配额冷却试探无意义
// （必然再被拒），严格跳过。
func probeEligible(e stateCacheEntry) bool {
	switch e.Class {
	case "auth", "rate_limit", "cooldown_until":
		return false
	}
	return e.Failures > 0
}

// acquireProbe Evaluate 侧抢全局试探锁；Redis 配置但故障回退 rand 兜底。
func (s *Service) acquireProbe(ctx context.Context, id string) bool {
	if s.Redis == nil {
		return false
	}
	ok, err := s.Redis.TryLock(ctx, "probe:"+id, probeLockTTL)
	if err != nil {
		return rand.Float64() < probeRate
	}
	return ok
}

// probeGranted 执行侧（Resolve/ExecuteKiro）核验在途试探：Evaluate 抢到锁后
// 本请求链路放行。降级时信任调度（Evaluate rand 放行的不二次掷骰）。
func (s *Service) probeGranted(ctx context.Context, id string, e stateCacheEntry) bool {
	if s.Redis == nil || !e.Found || !probeEligible(e) {
		return false
	}
	granted, err := s.Redis.Exists(ctx, "probe:"+id)
	if err != nil {
		return true
	}
	return granted
}

// entryFromModel 从 store 行构造缓存条目形态（执行侧核验复用 probeEligible）。
func entryFromModel(m *upstreamstore.Model) stateCacheEntry {
	e := stateCacheEntry{Found: true, Enabled: m.Enabled, Failures: m.Failures, Class: m.LastErrorClass, Account: m.Account, Window: m.ContextWindow, Native: m.NativeModel}
	if !m.CooldownUntil.IsZero() {
		e.Cooldown = m.CooldownUntil.Unix()
	}
	return e
}

// modelStates 取候选可用性态：Redis 可用按 id 读旁路（miss 批量回源一次
// ListModels 并回填），未配置/降级整批直读 DB（现行为）。
func (s *Service) modelStates(ctx context.Context, ids []string) ([]stateCacheEntry, error) {
	if s.Redis == nil {
		return s.modelStatesFromDB(ctx, ids)
	}
	entries := make([]stateCacheEntry, len(ids))
	var missing []int
	for i, id := range ids {
		v, found, err := s.Redis.Get(ctx, stateKey(id))
		if err != nil {
			return s.modelStatesFromDB(ctx, ids)
		}
		if found && json.Unmarshal([]byte(v), &entries[i]) == nil {
			continue
		}
		missing = append(missing, i)
	}
	if len(missing) > 0 {
		fromDB, err := s.modelStatesFromDB(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, i := range missing {
			entries[i] = fromDB[i]
			if b, err := json.Marshal(entries[i]); err == nil {
				_ = s.Redis.SetEx(ctx, stateKey(ids[i]), string(b), stateCacheTTL)
			}
		}
	}
	return entries, nil
}

// modelStatesFromDB 单次 ListModels 批量投影（DB 路径与缓存 miss 回源共用）。
func (s *Service) modelStatesFromDB(ctx context.Context, ids []string) ([]stateCacheEntry, error) {
	models, err := s.Store.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*upstreamstore.Model, len(models))
	for i := range models {
		byID[models[i].ID] = &models[i]
	}
	out := make([]stateCacheEntry, len(ids))
	for i, id := range ids {
		if m, ok := byID[id]; ok {
			out[i] = entryFromModel(m)
		}
	}
	return out, nil
}

// invalidateState 报告写路径主动失效（PG 权威 → 缓存让位；TTL 60s 兜底）。
func (s *Service) invalidateState(ctx context.Context, targetID string) {
	if s.Redis != nil {
		_ = s.Redis.Del(ctx, stateKey(targetID))
	}
}
