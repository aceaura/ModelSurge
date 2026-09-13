// Package schedule owns relay-side group policy and target-cache transitions.
package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"relayd/backend/contract/upstreamv1"
	"relayd/backend/relaystore"
)

type Upstream interface {
	Models(context.Context) ([]upstreamv1.ModelSummary, error)
	Resolve(context.Context, string) (upstreamv1.ResolvedTarget, error)
	Evaluate(context.Context, []string) ([]upstreamv1.CandidateEvaluation, error)
	Report(context.Context, upstreamv1.ResultReport) error
}

var ErrUnsupportedPolicy = errors.New("unsupported relay policy")

type Scheduler struct {
	Store    *relaystore.Store
	Upstream Upstream
	CacheTTL time.Duration

	mu      sync.Mutex
	cursors map[string]int
}
type Selection struct {
	GroupID string
	Target  upstreamv1.ResolvedTarget
}

func (s *Scheduler) Select(ctx context.Context, model string, tried map[string]bool) (Selection, error) {
	g, err := s.Store.GroupForModel(ctx, model)
	if err != nil {
		return Selection{}, err
	}
	if g == nil {
		return Selection{}, fmt.Errorf("user model %q not found", model)
	}
	cachePolicy := g.PolicyType == "" || g.PolicyType == "preset" || g.PolicyType == "sticky" || g.PolicyType == "failover"
	if cachePolicy && g.CachedTarget != "" && g.LastResult == "normal" && !tried[g.CachedTarget] && (s.CacheTTL <= 0 || time.Since(g.CacheUpdated) < s.CacheTTL) {
		t, err := s.Upstream.Resolve(ctx, g.CachedTarget)
		if err == nil {
			return Selection{g.ID, t}, nil
		}
		if cacheErr := s.Store.SetCache(ctx, g.ID, g.CachedTarget, "abnormal"); cacheErr != nil {
			return Selection{}, fmt.Errorf("mark unresolved cache abnormal: %w", cacheErr)
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
	evals, err := s.Upstream.Evaluate(ctx, ids)
	if err != nil {
		return Selection{}, err
	}
	ordered, err := s.order(g, ids, evals)
	if err != nil {
		return Selection{}, err
	}
	for _, e := range ordered {
		if !e.Available || tried[e.ID] {
			continue
		}
		t, err := s.Upstream.Resolve(ctx, e.ID)
		if err != nil {
			if cacheErr := s.Store.SetCache(ctx, g.ID, e.ID, "abnormal"); cacheErr != nil {
				return Selection{}, fmt.Errorf("resolve %s failed and cache update failed: %v; %w", e.ID, err, cacheErr)
			}
			continue
		}
		if err := s.Store.SetCache(ctx, g.ID, e.ID, "normal"); err != nil {
			return Selection{}, fmt.Errorf("cache selected target: %w", err)
		}
		return Selection{g.ID, t}, nil
	}
	return Selection{}, fmt.Errorf("no available target")
}

func (s *Scheduler) order(g *relaystore.Group, ids []string, evals []upstreamv1.CandidateEvaluation) ([]upstreamv1.CandidateEvaluation, error) {
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
		s.mu.Lock()
		if s.cursors == nil {
			s.cursors = map[string]int{}
		}
		start := s.cursors[g.ID] % len(ordered)
		s.cursors[g.ID] = (start + 1) % len(ordered)
		s.mu.Unlock()
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

func (s *Scheduler) Report(ctx context.Context, groupID string, t upstreamv1.ResolvedTarget, r upstreamv1.ResultReport) error {
	result := "abnormal"
	if r.Outcome == "normal" {
		result = "normal"
	}
	upErr := s.Upstream.Report(ctx, r)
	cacheErr := s.Store.SetCache(ctx, groupID, t.ID, result)
	if upErr != nil && cacheErr != nil {
		return fmt.Errorf("upstream report failed: %v; cache update failed: %w", upErr, cacheErr)
	}
	if upErr != nil {
		return upErr
	}
	return cacheErr
}
func (s *Scheduler) Models(ctx context.Context) ([]string, error) { return s.Store.Models(ctx) }

func (s *Scheduler) Authenticate(ctx context.Context, model, protocol, key string) (bool, bool, error) {
	return s.Store.Authenticate(ctx, model, protocol, key)
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
