// Package schedule owns relay-side group policy and target-cache transitions.
package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/redisx"
)

type Upstream interface {
	Models(context.Context) ([]upstreamv1.ModelSummary, error)
	Resolve(context.Context, string) (upstreamv1.ResolvedTarget, error)
	Evaluate(context.Context, []string) ([]upstreamv1.CandidateEvaluation, error)
	Report(context.Context, upstreamv1.ResultReport) (upstreamv1.ResultResponse, error)
	WebSearch(context.Context, upstreamv1.WebSearchRequest) (upstreamv1.WebSearchResponse, error)
}

var ErrUnsupportedPolicy = errors.New("unsupported relay policy")

// ErrContextTooLarge 估算 token 超过全部可用候选的上下文窗口（调度层
// 前置过滤；Agent 映射 413 给客户端）。
var ErrContextTooLarge = errors.New("context too large")

type Scheduler struct {
	Store               *relaystore.Store
	Upstream            Upstream
	CacheTTL            time.Duration
	AccessLogEnabled    bool
	AccessLogConfigured bool
	// Redis 可选热态层（round_robin 全局游标）；nil = 副本内局部轮询（现行为）。
	Redis *redisx.Client

	mu      sync.Mutex
	cursors map[string]int
}
type Selection struct {
	GroupID string
	Target  upstreamv1.ResolvedTarget
}

func (s *Scheduler) accessLog() bool { return !s.AccessLogConfigured || s.AccessLogEnabled }

func (s *Scheduler) Select(ctx context.Context, model string, tried map[string]bool, estTokens int) (Selection, error) {
	requestID := replayv1.RequestIDFromContext(ctx)
	g, err := s.Store.GroupForModel(ctx, model)
	if err != nil {
		return Selection{}, err
	}
	if g == nil {
		return Selection{}, fmt.Errorf("user model %q not found", model)
	}
	cachePolicy := g.PolicyType == "" || g.PolicyType == "preset" || g.PolicyType == "sticky" || g.PolicyType == "failover"
	cacheFresh := s.CacheTTL <= 0 || time.Since(g.CacheUpdated) < s.CacheTTL
	if s.accessLog() {
		state := "miss"
		if g.CachedTarget != "" && !cacheFresh {
			state = "stale"
		} else if cachePolicy && g.CachedTarget != "" && g.LastResult == "normal" && !tried[g.CachedTarget] {
			state = "hit"
		}
		log.Printf("replay phase=cache_%s request_id=%s group=%s target=%s policy=%s", state, requestID, g.ID, g.CachedTarget, g.PolicyType)
	}
	if cachePolicy && g.CachedTarget != "" && g.LastResult == "normal" && !tried[g.CachedTarget] && cacheFresh {
		started := time.Now()
		if s.accessLog() {
			log.Printf("replay phase=resolve_out request_id=%s target=%s", requestID, g.CachedTarget)
		}
		t, err := s.Upstream.Resolve(ctx, g.CachedTarget)
		if s.accessLog() {
			log.Printf("replay phase=resolve_in request_id=%s target=%s protocol=%s native_model=%s latency=%s error=%t", requestID, g.CachedTarget, t.Protocol, t.NativeModel, time.Since(started), err != nil)
		}
		if err == nil {
			// 窗口装不下时本次绕过缓存落评估（不清缓存：目标本身健康，
			// 只是这个请求太大）。
			if t.Runtime.MaxInputTokens > 0 && estTokens > t.Runtime.MaxInputTokens {
				if s.accessLog() {
					log.Printf("replay phase=cache_bypass request_id=%s target=%s reason=context_window est_tokens=%d max_input_tokens=%d", requestID, g.CachedTarget, estTokens, t.Runtime.MaxInputTokens)
				}
			} else {
				if s.accessLog() {
					log.Printf("replay phase=selected request_id=%s group=%s target=%s policy=%s protocol=%s native_model=%s", requestID, g.ID, t.ID, g.PolicyType, t.Protocol, t.NativeModel)
				}
				return Selection{g.ID, t}, nil
			}
		} else {
			if cacheErr := s.Store.SetCache(ctx, g.ID, g.CachedTarget, "abnormal"); cacheErr != nil {
				return Selection{}, fmt.Errorf("mark unresolved cache abnormal: %w", cacheErr)
			}
		}
	}
	ids := make([]string, 0, len(g.Members))
	for _, id := range g.Members {
		if !tried[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return Selection{}, fmt.Errorf("no eligible target")
	}
	evaluateStarted := time.Now()
	if s.accessLog() {
		log.Printf("replay phase=evaluate_out request_id=%s target_ids=%d", requestID, len(ids))
	}
	evals, err := s.Upstream.Evaluate(ctx, ids)
	available := 0
	for _, evaluation := range evals {
		if evaluation.Available {
			available++
		}
	}
	if s.accessLog() {
		log.Printf("replay phase=evaluate_in request_id=%s target_ids=%d available=%d latency=%s error=%t", requestID, len(ids), available, time.Since(evaluateStarted), err != nil)
	}
	if err != nil {
		return Selection{}, err
	}
	ordered, err := s.order(ctx, g, ids, evals)
	if err != nil {
		return Selection{}, err
	}
	excludedByWindow := 0
	for _, e := range ordered {
		if !e.Available || tried[e.ID] {
			continue
		}
		if e.ContextWindow > 0 && estTokens > e.ContextWindow {
			excludedByWindow++
			if s.accessLog() {
				log.Printf("replay phase=excluded request_id=%s target=%s reason=context_window est_tokens=%d context_window=%d", requestID, e.ID, estTokens, e.ContextWindow)
			}
			continue
		}
		resolveStarted := time.Now()
		if s.accessLog() {
			log.Printf("replay phase=resolve_out request_id=%s target=%s", requestID, e.ID)
		}
		t, err := s.Upstream.Resolve(ctx, e.ID)
		if s.accessLog() {
			log.Printf("replay phase=resolve_in request_id=%s target=%s protocol=%s native_model=%s latency=%s error=%t", requestID, e.ID, t.Protocol, t.NativeModel, time.Since(resolveStarted), err != nil)
		}
		if err != nil {
			if cacheErr := s.Store.SetCache(ctx, g.ID, e.ID, "abnormal"); cacheErr != nil {
				return Selection{}, fmt.Errorf("resolve %s failed and cache update failed: %v; %w", e.ID, err, cacheErr)
			}
			continue
		}
		if err := s.Store.SetCache(ctx, g.ID, e.ID, "normal"); err != nil {
			return Selection{}, fmt.Errorf("cache selected target: %w", err)
		}
		if s.accessLog() {
			log.Printf("replay phase=selected request_id=%s group=%s target=%s policy=%s protocol=%s native_model=%s", requestID, g.ID, t.ID, g.PolicyType, t.Protocol, t.NativeModel)
		}
		return Selection{g.ID, t}, nil
	}
	// 有可用候选但全被窗口排除：报超限（比笼统 no available target 更可诊断；
	// 无可用候选维持现状）。
	if excludedByWindow > 0 {
		return Selection{}, fmt.Errorf("estimated %d tokens exceed all candidate context windows: %w", estTokens, ErrContextTooLarge)
	}
	return Selection{}, fmt.Errorf("no available target")
}

func (s *Scheduler) order(ctx context.Context, g *relaystore.Group, ids []string, evals []upstreamv1.CandidateEvaluation) ([]upstreamv1.CandidateEvaluation, error) {
	byID := make(map[string]upstreamv1.CandidateEvaluation, len(evals))
	for _, e := range evals {
		byID[e.ID] = e
	}
	ordered := make([]upstreamv1.CandidateEvaluation, 0, len(ids))
	for _, id := range ids {
		e, ok := byID[id]
		if !ok {
			e = upstreamv1.CandidateEvaluation{ID: id, ExclusionReason: "missing_evaluation"}
		}
		ordered = append(ordered, e)
	}
	switch g.PolicyType {
	case "", "preset", "sticky", "failover":
		return ordered, nil
	case "round_robin":
		start := s.rrCursor(ctx, g.ID, len(ordered))
		return append(ordered[start:], ordered[:start]...), nil
	case "least_used":
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Score < ordered[j].Score })
		return ordered, nil
	case "dynamic":
		return nil, fmt.Errorf("%w: dynamic", ErrUnsupportedPolicy)
	default:
		var cfg map[string]any
		_ = json.Unmarshal([]byte(g.PolicyConfig), &cfg)
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedPolicy, g.PolicyType)
	}
}

// rrCursor round_robin 起始下标：Redis INCR 全局游标（多副本全局轮询，
// 设计 2.4）；未配置/降级回退副本内局部游标（设计 2.5 明示可接受）。
func (s *Scheduler) rrCursor(ctx context.Context, groupID string, n int) int {
	if s.Redis != nil {
		if v, err := s.Redis.Incr(ctx, "rr:"+groupID); err == nil {
			return int((v - 1) % int64(n))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursors == nil {
		s.cursors = map[string]int{}
	}
	start := s.cursors[groupID] % n
	s.cursors[groupID] = (start + 1) % n
	return start
}

func (s *Scheduler) Models(ctx context.Context) ([]string, error) { return s.Store.Models(ctx) }

func (s *Scheduler) Authenticate(ctx context.Context, model, protocol, key, compressOf string) (bool, bool, string, error) {
	return s.Store.Authenticate(ctx, model, protocol, key, compressOf)
}

func (s *Scheduler) ValidateMembers(ctx context.Context, ids []string) error {
	models, err := s.Upstream.Models(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(models))
	for _, m := range models {
		known[m.ID] = true
	}
	for _, id := range ids {
		if !known[id] {
			return fmt.Errorf("unknown upstream model %q", id)
		}
	}
	return nil
}
