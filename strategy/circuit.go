package strategy

import (
	"sync"
	"sync/atomic"
	"time"
)

type Status uint8

const (
	StatusEnabled Status = iota
	StatusManuallyDisabled
	StatusAutoDisabled
	StatusHalfOpen
)

func (s Status) String() string {
	switch s {
	case StatusEnabled:
		return "enabled"
	case StatusManuallyDisabled:
		return "manually_disabled"
	case StatusAutoDisabled:
		return "auto_disabled"
	case StatusHalfOpen:
		return "half_open"
	}
	return "unknown"
}

func (s Status) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

type CircuitConfig struct {
	FailThreshold  int
	OpenBase       time.Duration
	OpenMax        time.Duration
	HalfOpenProbes int
	SuccessToClose int
}

func (c CircuitConfig) withDefaults() CircuitConfig {
	if c.FailThreshold <= 0 {
		c.FailThreshold = 3
	}
	if c.OpenBase <= 0 {
		c.OpenBase = 30 * time.Second
	}
	if c.OpenMax <= 0 {
		c.OpenMax = 30 * time.Minute
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = 1
	}
	if c.SuccessToClose <= 0 {
		c.SuccessToClose = 2
	}
	return c
}

func NewCircuit(cfg CircuitConfig, manuallyDisabled bool, now func() time.Time, onTransition func(name string, d Decision)) *Circuit {
	if now == nil {
		now = time.Now
	}
	c := &Circuit{cfg: cfg.withDefaults(), now: now, onTransition: onTransition}
	c.status = StatusEnabled
	c.since = now()
	if manuallyDisabled {
		c.status = StatusManuallyDisabled
		c.reason = "disabled by config or admin"
	}
	return c
}

type Circuit struct {
	cfg          CircuitConfig
	now          func() time.Time
	onTransition func(name string, d Decision)
	name         string

	mu               sync.Mutex
	status           Status
	reason           string
	since            time.Time
	consecFail       int
	openUntil        time.Time
	backoffLevel     int
	cooldownUntil    time.Time
	autoBan          bool
	halfOpenInFlight atomic.Int32
	halfOpenSuccess  int
	lastBalance      *float64
	lastBalanceAt    time.Time
	requests         int64
	successes        int64
	lastLatency      time.Duration
}

func (c *Circuit) SetName(name string) { c.name = name }

func (c *Circuit) SetAutoBan(v bool) { c.autoBan = v }

func (c *Circuit) transitionLocked(to Status, reason string, at time.Time) Decision {
	d := Decision{From: c.status, To: to, Changed: c.status != to, Reason: reason}
	c.status = to
	c.reason = reason
	c.since = at
	if d.Changed && c.onTransition != nil {
		c.onTransition(c.name, d)
	}
	return d
}

func (c *Circuit) Feed(s Signal) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := s.At
	if at.IsZero() {
		at = c.now()
	}

	if c.status == StatusManuallyDisabled {
		return Decision{From: c.status, To: c.status}
	}

	if s.Kind == KindTraffic {
		c.requests++
		if s.OK {
			c.successes++
		}
		c.lastLatency = s.Latency
	}

	if s.Balance != nil {
		v := *s.Balance
		c.lastBalance = &v
		c.lastBalanceAt = at
	}

	if s.RateLimit != nil {
		d := clampDuration(s.RateLimit.RetryAfter, 5*time.Second, 30*time.Minute)
		c.cooldownUntil = at.Add(d)
		dec := Decision{From: c.status, To: c.status, Reason: "rate limited, cooldown " + d.String()}
		if c.onTransition != nil {
			c.onTransition(c.name, dec)
		}
		return dec
	}

	if s.Fatal {
		c.openUntil = at.Add(c.cfg.OpenMax)
		c.consecFail = 0
		return c.transitionLocked(StatusAutoDisabled, nonEmpty(s.Reason, "fatal signal"), at)
	}

	if !s.OK {
		if c.status == StatusHalfOpen {
			c.backoffLevel++
			c.halfOpenSuccess = 0
			c.openUntil = at.Add(c.backoff())
			return c.transitionLocked(StatusAutoDisabled, nonEmpty(s.Reason, "half-open probe failed"), at)
		}
		c.consecFail++
		if c.consecFail >= c.cfg.FailThreshold && c.autoBan {
			c.openUntil = at.Add(c.backoff())
			c.backoffLevel++
			return c.transitionLocked(StatusAutoDisabled, nonEmpty(s.Reason, "consecutive failures"), at)
		}
		return Decision{From: c.status, To: c.status, Reason: s.Reason}
	}

	switch c.status {
	case StatusHalfOpen:
		c.halfOpenSuccess++
		if c.halfOpenSuccess >= c.cfg.SuccessToClose {
			if c.backoffLevel > 0 {
				c.backoffLevel--
			}
			c.consecFail = 0
			c.halfOpenSuccess = 0
			return c.transitionLocked(StatusEnabled, "recovered", at)
		}
		return Decision{From: c.status, To: c.status, Reason: "half-open success"}
	case StatusAutoDisabled:
		if s.Kind != KindTraffic {
			// fresh probe evidence while disabled: give it a half-open trial
			c.halfOpenSuccess = 0
			return c.transitionLocked(StatusHalfOpen, "probe succeeded: "+nonEmpty(s.Reason, "ok"), at)
		}
		c.consecFail = 0
		return Decision{From: c.status, To: c.status, Reason: s.Reason}
	default:
		c.consecFail = 0
		return Decision{From: c.status, To: c.status, Reason: s.Reason}
	}
}

func (c *Circuit) backoff() time.Duration {
	d := c.cfg.OpenBase
	for i := 0; i < c.backoffLevel; i++ {
		d *= 2
		if d >= c.cfg.OpenMax {
			return c.cfg.OpenMax
		}
	}
	return min(d, c.cfg.OpenMax)
}

func (c *Circuit) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.availableLocked(c.now())
}

func (c *Circuit) availableLocked(now time.Time) bool {
	if now.Before(c.cooldownUntil) {
		return false
	}
	switch c.status {
	case StatusEnabled:
		return true
	case StatusAutoDisabled:
		if !now.Before(c.openUntil) {
			c.transitionLocked(StatusHalfOpen, "open window elapsed", now)
			return true
		}
		return false
	case StatusHalfOpen:
		return true
	default:
		return false
	}
}

func (c *Circuit) TryAcquireHalfOpen() bool {
	c.mu.Lock()
	st := c.status
	c.mu.Unlock()
	if st != StatusHalfOpen {
		return true
	}
	for {
		v := c.halfOpenInFlight.Load()
		if v >= int32(c.cfg.HalfOpenProbes) {
			return false
		}
		if c.halfOpenInFlight.CompareAndSwap(v, v+1) {
			return true
		}
	}
}

func (c *Circuit) ReleaseHalfOpen() {
	c.mu.Lock()
	st := c.status
	c.mu.Unlock()
	if st == StatusHalfOpen {
		c.halfOpenInFlight.Add(-1)
	}
}

func (c *Circuit) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// ManualEnable forces Enabled and clears backoff memory.
func (c *Circuit) ManualEnable() Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backoffLevel = 0
	c.consecFail = 0
	c.halfOpenSuccess = 0
	c.cooldownUntil = time.Time{}
	return c.transitionLocked(StatusEnabled, "manual enable", c.now())
}

// ManualDisable forces ManuallyDisabled; automatic logic never revives it.
func (c *Circuit) ManualDisable(reason string) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.transitionLocked(StatusManuallyDisabled, nonEmpty(reason, "manual disable"), c.now())
}

type Snapshot struct {
	Status        Status         `json:"status"`
	Reason        string         `json:"reason"`
	Since         time.Time      `json:"since"`
	ConsecFail    int            `json:"consec_fail"`
	OpenUntil     *time.Time     `json:"open_until,omitempty"`
	CooldownUntil *time.Time     `json:"cooldown_until,omitempty"`
	BackoffLevel  int            `json:"backoff_level"`
	LastBalance   *float64       `json:"last_balance,omitempty"`
	LastBalanceAt *time.Time     `json:"last_balance_at,omitempty"`
	Requests      int64          `json:"requests"`
	Successes     int64          `json:"successes"`
	LastLatencyMs int64          `json:"last_latency_ms,omitempty"`
}

func nonZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (c *Circuit) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{
		Status:        c.status,
		Reason:        c.reason,
		Since:         c.since,
		ConsecFail:    c.consecFail,
		OpenUntil:     nonZero(c.openUntil),
		CooldownUntil: nonZero(c.cooldownUntil),
		BackoffLevel:  c.backoffLevel,
		LastBalance:   c.lastBalance,
		LastBalanceAt: nonZero(c.lastBalanceAt),
		Requests:      c.requests,
		Successes:     c.successes,
		LastLatencyMs: c.lastLatency.Milliseconds(),
	}
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
