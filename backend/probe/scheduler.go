package probe

import (
	"container/heap"
	"context"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"relayd/strategy"
)

var probeHTTPClient = &http.Client{Timeout: 30 * time.Second}

// Scheduler runs per-upstream probes with jitter, a global rate budget and a
// fixed worker pool. Manually disabled upstreams are never scheduled.
type Scheduler struct {
	concurrency int
	jitter      float64
	budget      int    // tokens per minute
	mode        string // "all" | "passive_recovery"

	mu     sync.Mutex
	items  probeHeap
	notify chan struct{}
}

type job struct {
	u        *strategy.Upstream
	p        Prober
	next     time.Time
	interval time.Duration
	index    int
}

type probeHeap []*job

func (h probeHeap) Len() int           { return len(h) }
func (h probeHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }
func (h probeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *probeHeap) Push(x any)        { *h = append(*h, x.(*job)) }
func (h *probeHeap) Pop() any {
	old := *h
	n := len(old)
	j := old[n-1]
	*h = old[:n-1]
	return j
}

func NewScheduler(cfg SchedulerConfig) *Scheduler {
	s := &Scheduler{
		concurrency: cfg.Concurrency,
		jitter:      cfg.Jitter,
		budget:      cfg.BudgetPerMinute,
		mode:        cfg.Mode,
		notify:      make(chan struct{}, 1),
	}
	if s.concurrency <= 0 {
		s.concurrency = 8
	}
	if s.budget <= 0 {
		s.budget = 60
	}
	if s.mode == "" {
		s.mode = "all"
	}
	return s
}

type SchedulerConfig struct {
	Concurrency     int
	Jitter          float64
	BudgetPerMinute int
	Mode            string
}

// Register adds (upstream, prober) pairs; replaces any existing registration.
func (s *Scheduler) Register(u *strategy.Upstream, probers []Prober) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range probers {
		interval := p.Interval()
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		heap.Push(&s.items, &job{
			u:        u,
			p:        p,
			next:     time.Now().Add(s.jittered(interval)),
			interval: interval,
		})
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Scheduler) jittered(base time.Duration) time.Duration {
	if s.jitter <= 0 {
		return base
	}
	delta := (rand.Float64()*2 - 1) * s.jitter
	return time.Duration(float64(base) * (1 + delta))
}

func (s *Scheduler) eligible(u *strategy.Upstream) bool {
	st := u.Circuit.Status()
	if st == strategy.StatusManuallyDisabled {
		return false
	}
	if s.mode == "passive_recovery" {
		return st == strategy.StatusAutoDisabled || st == strategy.StatusHalfOpen
	}
	return true
}

// Run blocks until ctx cancels. Budget: token bucket refilled per minute.
func (s *Scheduler) Run(ctx context.Context) {
	tokens := make(chan struct{}, s.budget)
	go func() {
		tick := time.NewTicker(time.Minute / time.Duration(max(s.budget, 1)))
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				select {
				case tokens <- struct{}{}:
				default:
				}
			}
		}
	}()

	work := make(chan *job)
	var wg sync.WaitGroup
	for i := 0; i < s.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-work:
					s.execute(ctx, j)
				}
			}
		}()
	}

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.mu.Lock()
		wait := time.Hour
		if s.items.Len() > 0 {
			wait = max(time.Until(s.items[0].next), 0)
		}
		s.mu.Unlock()

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-s.notify:
			continue
		case <-timer.C:
			now := time.Now()
			var due []*job
			s.mu.Lock()
			for s.items.Len() > 0 && !s.items[0].next.After(now) {
				j := heap.Pop(&s.items).(*job)
				j.next = now.Add(s.jittered(j.interval))
				heap.Push(&s.items, j)
				due = append(due, j)
			}
			s.mu.Unlock()
			for _, j := range due {
				if !s.eligible(j.u) {
					continue
				}
				select {
				case <-tokens:
				default:
					continue // over budget: skip, no catch-up
				}
				select {
				case work <- j:
				case <-ctx.Done():
					wg.Wait()
					return
				}
			}
		}
	}
}

func (s *Scheduler) execute(ctx context.Context, j *job) {
	t := Target{
		Name:    j.u.Name,
		BaseURL: j.u.BaseURL,
		APIKey:  j.u.APIKey,
		Models:  j.u.Models,
		Native:  j.u.NativeModel,
		Client:  probeHTTPClient,
	}
	sig := j.p.Probe(ctx, t)
	j.u.Feed(sig)
}
