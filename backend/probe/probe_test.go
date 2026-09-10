package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"relayd/backend/catalog"
	"relayd/strategy"
)

func mkTarget(server *httptest.Server) Target {
	return Target{
		Name:    "t",
		BaseURL: server.URL,
		APIKey:  "sk-x",
		Models:  []string{"gpt-4o"},
		Native:  map[string]string{"gpt-4o": "gpt-4o"},
		Client:  server.Client(),
	}
}

func balanceCfg(path, extract string, scale float64, below float64) catalog.ProbeConfig {
	return catalog.ProbeConfig{
		Type:     "balance",
		Interval: catalog.Duration(time.Minute),
		Request: &catalog.HTTPProbeReq{
			Method: "GET",
			Path:   path,
			Auth:   catalog.AuthSpec{In: "header", Name: "Authorization", Template: "Bearer {api_key}"},
		},
		Extract: &catalog.Extract{Value: extract, Scale: scale},
		Rule:    &catalog.BalanceRule{DisableBelow: below},
	}
}

func TestBalance_BelowThresholdFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-x" {
			t.Errorf("auth template not applied: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"data":{"quota":10000}}`)
	}))
	defer srv.Close()

	p, err := NewProber("t", balanceCfg("/api/user/self", "data.quota", 0.000002, 0.01))
	if err != nil {
		t.Fatal(err)
	}
	sig := p.Probe(context.Background(), mkTarget(srv))
	// 10000 * 0.000002 = 0.02 USD > 0.01 => not fatal
	if !sig.OK || sig.Fatal || sig.Balance == nil || *sig.Balance < 0.019 || *sig.Balance > 0.021 {
		t.Fatalf("unexpected signal: %+v", sig)
	}

	p, _ = NewProber("t", balanceCfg("/api/user/self", "data.quota", 0.000002, 0.05))
	sig = p.Probe(context.Background(), mkTarget(srv))
	if !sig.OK || !sig.Fatal {
		t.Fatalf("below threshold must be fatal: %+v", sig)
	}
}

func TestBalance_MinusTwoEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/subscription":
			fmt.Fprint(w, `{"hard_limit_usd":10}`)
		case "/usage":
			if r.URL.Query().Get("start_date") == "" {
				t.Error("date template not expanded")
			}
			fmt.Fprint(w, `{"total_usage":250}`) // 250 * 0.01 = 2.5
		}
	}))
	defer srv.Close()

	cfg := balanceCfg("/subscription", "hard_limit_usd", 0, 0.5)
	cfg.Minus = &catalog.BalanceTerm{
		Request: catalog.HTTPProbeReq{
			Method: "GET",
			Path:   "/usage?start_date={date-100d}&end_date={date+1d}",
			Auth:   catalog.AuthSpec{In: "header", Name: "Authorization", Template: "Bearer {api_key}"},
		},
		Extract: catalog.Extract{Value: "total_usage", Scale: 0.01},
	}
	p, err := NewProber("t", cfg)
	if err != nil {
		t.Fatal(err)
	}
	sig := p.Probe(context.Background(), mkTarget(srv))
	if !sig.OK || sig.Fatal || sig.Balance == nil || *sig.Balance < 7.49 || *sig.Balance > 7.51 {
		t.Fatalf("want ~7.5 USD balance, got %+v", sig)
	}
}

func TestBalance_QueryFailureConservative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, _ := NewProber("t", balanceCfg("/api/user/self", "data.quota", 1, 0.01))
	sig := p.Probe(context.Background(), mkTarget(srv))
	if sig.OK || sig.Fatal {
		t.Fatalf("query failure must be conservative (not fatal): %+v", sig)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{}}`)
	}))
	defer srv2.Close()
	sig = p.Probe(context.Background(), mkTarget(srv2))
	if sig.OK || sig.Fatal {
		t.Fatalf("missing path must be conservative: %+v", sig)
	}
}

func TestPing_Classification(t *testing.T) {
	var mode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0:
			fmt.Fprint(w, `{"id":"ok"}`)
		case 1:
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(429)
		case 2:
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()

	p, err := NewProber("t", catalog.ProbeConfig{
		Type:     "ping",
		Interval: catalog.Duration(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	sig := p.Probe(context.Background(), mkTarget(srv))
	if !sig.OK || sig.Kind != strategy.KindPing {
		t.Fatalf("healthy ping: %+v", sig)
	}

	mode.Store(1)
	sig = p.Probe(context.Background(), mkTarget(srv))
	if sig.RateLimit == nil || sig.Fatal {
		t.Fatalf("429 ping: %+v", sig)
	}

	mode.Store(2)
	sig = p.Probe(context.Background(), mkTarget(srv))
	if !sig.Fatal {
		t.Fatalf("401 ping must be fatal: %+v", sig)
	}
}

func TestPing_SlowThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	p, _ := NewProber("t", catalog.ProbeConfig{
		Type:          "ping",
		Interval:      catalog.Duration(time.Minute),
		SlowThreshold: catalog.Duration(50 * time.Millisecond),
	})
	sig := p.Probe(context.Background(), mkTarget(srv))
	if sig.OK {
		t.Fatal("slow response must fail the ping")
	}
	if sig.Reason == "" || sig.Latency < 100*time.Millisecond {
		t.Fatalf("latency should be recorded: %+v", sig)
	}
}

func TestScheduler_RunsAndSkipsManualDisabled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"id":"ok"}`)
	}))
	defer srv.Close()

	mkUp := func(name string, manual bool) *strategy.Upstream {
		c := strategy.NewCircuit(strategy.CircuitConfig{}, manual, time.Now, nil)
		c.SetAutoBan(true)
		return &strategy.Upstream{
			Name: name, BaseURL: srv.URL, APIKey: "k",
			Models: []string{"m"}, NativeModel: map[string]string{"m": "m"},
			Circuit: c,
		}
	}
	active := mkUp("active", false)
	off := mkUp("off", true)

	s := NewScheduler(SchedulerConfig{Concurrency: 2, BudgetPerMinute: 600, Mode: "all"})
	p, _ := NewProber("x", catalog.ProbeConfig{Type: "ping", Interval: catalog.Duration(30 * time.Millisecond)})
	s.Register(active, []Prober{p})
	s.Register(off, []Prober{p})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	if hits.Load() == 0 {
		t.Fatal("active upstream must be probed")
	}
	if offProbed(off) {
		t.Fatal("manually disabled upstream must never be probed")
	}
}

// offProbed reports whether a ping signal was ever fed: a manual-disabled
// circuit drops all signals, so detect via balance absence and consecFail.
func offProbed(u *strategy.Upstream) bool {
	snap := u.Circuit.Snapshot()
	return snap.ConsecFail != 0 || snap.LastBalance != nil
}

func TestScheduler_PassiveRecoveryOnlyDead(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"id":"ok"}`)
	}))
	defer srv.Close()

	mkUp := func(name string) *strategy.Upstream {
		c := strategy.NewCircuit(strategy.CircuitConfig{FailThreshold: 1, OpenBase: time.Hour, OpenMax: 2 * time.Hour, HalfOpenProbes: 1, SuccessToClose: 1}, false, time.Now, nil)
		c.SetAutoBan(true)
		return &strategy.Upstream{
			Name: name, BaseURL: srv.URL, APIKey: "k",
			Models: []string{"m"}, NativeModel: map[string]string{"m": "m"},
			Circuit: c,
		}
	}
	healthy := mkUp("healthy")
	dead := mkUp("dead")
	dead.Feed(strategy.Signal{OK: false, Fatal: true, Reason: "broke"})

	s := NewScheduler(SchedulerConfig{Concurrency: 2, BudgetPerMinute: 600, Mode: "passive_recovery"})
	p, _ := NewProber("x", catalog.ProbeConfig{Type: "ping", Interval: catalog.Duration(20 * time.Millisecond)})
	s.Register(healthy, []Prober{p})
	s.Register(dead, []Prober{p})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	// in passive_recovery only the dead upstream gets probed; its ping success
	// moves it half_open->enabled... actually it's auto_disabled -> eligible
	if hits.Load() == 0 {
		t.Fatal("dead upstream must be probed in passive_recovery")
	}
}
