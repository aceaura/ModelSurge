package strategy

import (
	"slices"
	"sync"
)

// Registry indexes upstreams by canonical model name.
// The whole map is replaced copy-on-write on (re)load.
type Registry struct {
	mu      sync.RWMutex
	byModel map[string][]*Upstream
	all     []*Upstream
}

func NewRegistry() *Registry {
	return &Registry{byModel: map[string][]*Upstream{}}
}

// Replace rebuilds the index from the given upstream set.
func (r *Registry) Replace(all []*Upstream) {
	byModel := map[string][]*Upstream{}
	for _, u := range all {
		for _, m := range u.Models {
			byModel[m] = append(byModel[m], u)
		}
	}
	for _, list := range byModel {
		slices.SortStableFunc(list, func(a, b *Upstream) int { return a.Priority - b.Priority })
	}
	r.mu.Lock()
	r.byModel = byModel
	r.all = all
	r.mu.Unlock()
}

func (r *Registry) All() []*Upstream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.all)
}

func (r *Registry) Candidates(canonical string) []*Upstream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.byModel[canonical])
}

// Pick selects one upstream for canonical: first priority tier with an
// available candidate, smooth weighted round-robin inside the tier.
func (r *Registry) Pick(canonical string, exclude map[*Upstream]bool) *Upstream {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.byModel[canonical]
	for tierStart := 0; tierStart < len(list); {
		tierEnd := tierStart
		for tierEnd < len(list) && list[tierEnd].Priority == list[tierStart].Priority {
			tierEnd++
		}
		if u := pickSWRR(list[tierStart:tierEnd], exclude); u != nil {
			return u
		}
		tierStart = tierEnd
	}
	return nil
}

// pickSWRR returns the chosen upstream. If it is half-open, a probe permit
// has been acquired; the caller MUST call Circuit.ReleaseHalfOpen when the
// attempt finishes.
func pickSWRR(tier []*Upstream, exclude map[*Upstream]bool) *Upstream {
	skip := map[*Upstream]bool{}
	for {
		var best *Upstream
		var total int64
		for _, u := range tier {
			if exclude[u] || skip[u] || !u.Available() {
				continue
			}
			w := int64(max(u.Weight, 1))
			total += w
			cw := u.currentWeight.Add(w)
			if best == nil || cw > best.currentWeight.Load() {
				best = u
			}
		}
		if best == nil {
			return nil
		}
		best.currentWeight.Add(-total)
		if best.Circuit.TryAcquireHalfOpen() {
			return best
		}
		// half-open permit exhausted; undo the SWRR debit and try again
		best.currentWeight.Add(total)
		skip[best] = true
	}
}
