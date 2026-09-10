package strategy

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newTestCircuit(fc *fakeClock) *Circuit {
	c := NewCircuit(CircuitConfig{
		FailThreshold:  3,
		OpenBase:       30 * time.Second,
		OpenMax:        10 * time.Minute,
		HalfOpenProbes: 1,
		SuccessToClose: 2,
	}, false, fc.now, nil)
	c.SetAutoBan(true)
	return c
}

func TestFeed_ConsecutiveFailuresTripCircuit(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)

	for i := 0; i < 2; i++ {
		d := c.Feed(Signal{Kind: KindTraffic, OK: false, Reason: "boom"})
		if d.Changed {
			t.Fatalf("attempt %d: unexpected transition %+v", i+1, d)
		}
	}
	d := c.Feed(Signal{Kind: KindTraffic, OK: false, Reason: "boom"})
	if !d.Changed || d.To != StatusAutoDisabled {
		t.Fatalf("want AutoDisabled after 3 failures, got %+v", d)
	}
	if c.Available() {
		t.Fatal("must not be available inside open window")
	}

	fc.advance(30 * time.Second)
	if !c.Available() {
		t.Fatal("open_base elapsed: must lazily become available (half-open)")
	}
	if c.Status() != StatusHalfOpen {
		t.Fatalf("want HalfOpen, got %s", c.Status())
	}
}

func TestFeed_HalfOpenRecoveryAndBackoffMemory(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)
	for i := 0; i < 3; i++ {
		c.Feed(Signal{OK: false})
	}
	fc.advance(31 * time.Second)
	c.Available() // -> half-open

	c.Feed(Signal{OK: true})
	if c.Status() != StatusHalfOpen {
		t.Fatal("one success must not close; success_to_close=2")
	}
	c.Feed(Signal{OK: true})
	if c.Status() != StatusEnabled {
		t.Fatalf("want Enabled after 2 successes, got %s", c.Status())
	}
	if c.Snapshot().BackoffLevel != 0 {
		t.Fatalf("backoff should drop one level from 1 to 0, got %d", c.Snapshot().BackoffLevel)
	}
}

func TestFeed_HalfOpenFailureRaisesBackoff(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)
	for i := 0; i < 3; i++ {
		c.Feed(Signal{OK: false})
	}
	fc.advance(31 * time.Second)
	c.Available()

	c.Feed(Signal{OK: false, Reason: "still bad"})
	if c.Status() != StatusAutoDisabled {
		t.Fatalf("half-open failure must reopen, got %s", c.Status())
	}
	// backoffLevel: 1 after trip, 2 after reopen => window = 30s<<2 = 120s
	fc.advance(100 * time.Second)
	if c.Available() {
		t.Fatal("still inside raised open window (120s)")
	}
	fc.advance(21 * time.Second)
	if !c.Available() {
		t.Fatal("raised open window elapsed")
	}
}

func TestFeed_RateLimitCooldownOnly(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)

	d := c.Feed(Signal{OK: false, RateLimit: &RateLimit{RetryAfter: 10 * time.Second, Source: "retry-after"}})
	if d.Changed {
		t.Fatalf("429 must not change status, got %+v", d)
	}
	if c.Available() {
		t.Fatal("cooldown active: must be unavailable")
	}
	if c.Snapshot().ConsecFail != 0 {
		t.Fatal("429 must not count as consecutive failure")
	}
	fc.advance(11 * time.Second)
	if !c.Available() {
		t.Fatal("cooldown elapsed")
	}
	// clamp: 1s -> 5s
	c.Feed(Signal{RateLimit: &RateLimit{RetryAfter: time.Second}})
	if c.Available() {
		t.Fatal("clamped 5s cooldown must apply")
	}
	fc.advance(3 * time.Second)
	if c.Available() {
		t.Fatal("still within clamped cooldown")
	}
}

func TestFeed_FatalUsesOpenMax(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)
	c.Feed(Signal{OK: false, Fatal: true, Reason: "credit balance is too low"})
	if c.Status() != StatusAutoDisabled {
		t.Fatalf("want AutoDisabled, got %s", c.Status())
	}
	fc.advance(5 * time.Minute)
	if c.Available() {
		t.Fatal("fatal errors use open_max (10m), not open_base")
	}
	fc.advance(6 * time.Minute)
	if !c.Available() {
		t.Fatal("open_max elapsed")
	}
}

func TestFeed_ManuallyDisabledIgnoresEverything(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := NewCircuit(CircuitConfig{}, true, fc.now, nil)
	c.SetAutoBan(true)

	for _, s := range []Signal{
		{OK: false, Fatal: true},
		{OK: false},
		{OK: true},
		{RateLimit: &RateLimit{RetryAfter: time.Minute}},
	} {
		d := c.Feed(s)
		if d.Changed || c.Status() != StatusManuallyDisabled {
			t.Fatalf("manual disabled must never auto-transition, got %+v status=%s", d, c.Status())
		}
	}
	if c.Available() {
		t.Fatal("manually disabled must never be available")
	}
}

func TestFeed_AutoBanDisabledNeverTrips(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)
	c.SetAutoBan(false)
	for i := 0; i < 10; i++ {
		d := c.Feed(Signal{OK: false})
		if d.Changed {
			t.Fatalf("auto_ban=false must veto circuit trip, got %+v", d)
		}
	}
	// fatal still disables (explicit ban-worthy signal)
	d := c.Feed(Signal{OK: false, Fatal: true})
	if !d.Changed || d.To != StatusAutoDisabled {
		t.Fatalf("fatal must still disable, got %+v", d)
	}
}

func TestFeed_BalanceSnapshotRecorded(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)
	b := 4.2
	c.Feed(Signal{Kind: KindBalance, OK: true, Balance: &b})
	snap := c.Snapshot()
	if snap.LastBalance == nil || *snap.LastBalance != 4.2 {
		t.Fatalf("balance snapshot lost: %+v", snap)
	}
}

func TestManualOverride(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)
	c.ManualDisable("test")
	if c.Status() != StatusManuallyDisabled {
		t.Fatal("manual disable failed")
	}
	d := c.ManualEnable()
	if !d.Changed || c.Status() != StatusEnabled || c.Snapshot().BackoffLevel != 0 {
		t.Fatalf("manual enable must reset backoff, got %+v", d)
	}
}

func TestFeed_RateLimitEmitsEventAndCounters(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	var events []Decision
	c := NewCircuit(CircuitConfig{}, false, fc.now, func(name string, d Decision) {
		events = append(events, d)
	})
	c.SetAutoBan(true)

	d := c.Feed(Signal{OK: false, RateLimit: &RateLimit{RetryAfter: time.Minute}})
	if d.Changed {
		t.Fatal("rate limit must not change status")
	}
	if len(events) != 1 || events[0].Reason == "" {
		t.Fatalf("rate limit must emit an event, got %+v", events)
	}

	c.Feed(Signal{Kind: KindTraffic, OK: true, Latency: 120 * time.Millisecond})
	c.Feed(Signal{Kind: KindTraffic, OK: false, Latency: 80 * time.Millisecond})
	snap := c.Snapshot()
	if snap.Requests != 2 || snap.Successes != 1 {
		t.Fatalf("counters wrong: %+v", snap)
	}
	if snap.LastLatencyMs != 80 {
		t.Fatalf("last latency wrong: %d", snap.LastLatencyMs)
	}
	if snap.OpenUntil != nil || snap.LastBalanceAt != nil {
		t.Fatal("unset times must serialize as nil")
	}
}

func TestFeed_ProbeRecoversAutoDisabled(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)

	b := 0.0
	c.Feed(Signal{Kind: KindBalance, OK: true, Balance: &b, Fatal: true, Reason: "balance below threshold"})
	if c.Status() != StatusAutoDisabled {
		t.Fatalf("fatal balance must disable, got %s", c.Status())
	}

	// traffic OK must not revive (no fresh probe evidence)
	c.Feed(Signal{Kind: KindTraffic, OK: true})
	if c.Status() != StatusAutoDisabled {
		t.Fatalf("traffic OK must not recover auto-disabled, got %s", c.Status())
	}

	// refilled: successful balance probe flips to half-open immediately,
	// without waiting out the 10m open window
	b = 100.0
	d := c.Feed(Signal{Kind: KindBalance, OK: true, Balance: &b})
	if !d.Changed || d.To != StatusHalfOpen {
		t.Fatalf("probe OK must half-open, got %+v", d)
	}
	c.Feed(Signal{Kind: KindBalance, OK: true, Balance: &b})
	c.Feed(Signal{Kind: KindBalance, OK: true, Balance: &b})
	if c.Status() != StatusEnabled {
		t.Fatalf("want Enabled after success_to_close probe successes, got %s", c.Status())
	}
}

func TestHalfOpenPermitLimit(t *testing.T) {
	fc := &fakeClock{t: time.Now()}
	c := newTestCircuit(fc)
	for i := 0; i < 3; i++ {
		c.Feed(Signal{OK: false})
	}
	fc.advance(31 * time.Second)
	c.Available()

	if !c.TryAcquireHalfOpen() {
		t.Fatal("first permit must succeed")
	}
	if c.TryAcquireHalfOpen() {
		t.Fatal("second permit must fail (half_open_probes=1)")
	}
	c.ReleaseHalfOpen()
	if !c.TryAcquireHalfOpen() {
		t.Fatal("permit must be reusable after release")
	}
}
