package relayd_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relayd/backend/admin"
	"relayd/backend/catalog"
	"relayd/backend/egress"
	"relayd/backend/ingress"
	"relayd/backend/obs"
	"relayd/backend/probe"
	"relayd/strategy"
)

// e2e stack: station (balance probe, quota drains to zero), direct
// (ping probe), openai-style (minus balance). Traffic for gpt-4o prefers
// station; when its balance drops below threshold it must be auto-disabled
// and traffic must shift to direct.

type scriptedUpstream struct {
	srv  *httptest.Server
	hits atomic.Int32
}

func TestEndToEnd_BalanceExhaustionFailover(t *testing.T) {
	var stationQuota atomic.Int64
	stationQuota.Store(500000) // 500000 * 0.000002 = 1.0 USD

	station := &scriptedUpstream{}
	station.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/user/self") {
			fmt.Fprintf(w, `{"data":{"quota":%d}}`, stationQuota.Load())
			return
		}
		station.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"station","choices":[{"message":{"role":"assistant","content":"from-station"}}]}`)
	}))
	defer station.srv.Close()

	direct := &scriptedUpstream{}
	direct.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"direct","choices":[{"message":{"role":"assistant","content":"from-direct"}}]}`)
	}))
	defer direct.srv.Close()

	yaml := fmt.Sprintf(`
listen: "127.0.0.1:18080"
admin_listen: "127.0.0.1:18081"
auth_tokens: ["tok"]
circuit:
  fail_threshold: 3
  open_base: 100ms
  open_max: 5s
  half_open_probes: 1
  success_to_close: 1
models:
  gpt-4o: {protocol_hint: openai}
providers:
  station:
    protocol: openai
    probes:
      - type: balance
        interval: 50ms
        request:
          method: GET
          path: /api/user/self
          auth: {in: header, name: Authorization, template: "Bearer {api_key}"}
        extract: {value: "data.quota", scale: 0.000002}
        rule: {disable_below: 0.01}
  direct:
    protocol: openai
    probes:
      - type: ping
        interval: 5m
credentials:
  - provider: station
    name: st-01
    base_url: %s
    api_key: sk-station
    models: [gpt-4o]
    priority: 0
    weight: 100
  - provider: direct
    name: di-01
    base_url: %s
    api_key: sk-direct
    models: [gpt-4o]
    priority: 1
    weight: 1
`, station.srv.URL, direct.srv.URL)

	cfgPath := filepath.Join(t.TempDir(), "relayd.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := catalog.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	ring := obs.NewRing(64)
	cat := catalog.NewCatalog(cfg.Models)
	ups, err := cfg.BuildUpstreams(cat, func(name string, d strategy.Decision) {
		ring.Add(obs.Event{Upstream: name, From: d.From.String(), To: d.To.String(), Reason: d.Reason})
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := strategy.NewRegistry()
	reg.Replace(ups)

	h := &ingress.Handler{
		Reg: reg, Catalog: cat, Fwd: egress.NewForwarder(),
		Tokens: map[string]bool{"tok": true}, MaxBody: 1 << 20,
	}
	biz := httptest.NewServer(h.Mux())
	defer biz.Close()

	sched := probe.NewScheduler(probe.SchedulerConfig{Concurrency: 2, BudgetPerMinute: 6000, Mode: "all"})
	probersByUpstream := map[string][]probe.Prober{}
	for _, u := range ups {
		var ps []probe.Prober
		for _, pc := range cfg.ProbesFor(u.Name) {
			p, err := probe.NewProber(u.Name, pc)
			if err != nil {
				t.Fatal(err)
			}
			ps = append(ps, p)
		}
		probersByUpstream[u.Name] = ps
		sched.Register(u, ps)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.Run(ctx)

	adm := &admin.Server{
		Reg: reg, Ring: ring, Tokens: map[string]bool{"tok": true},
		ProberOf: func(name string) []probe.Prober { return probersByUpstream[name] },
	}
	adminSrv := httptest.NewServer(adm.Mux())
	defer adminSrv.Close()

	chat := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, biz.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// phase 1: station has balance, serves traffic
	code, body := chat()
	if code != 200 || !strings.Contains(body, "from-station") {
		t.Fatalf("phase1: want station 200, got %d %s", code, body)
	}

	// phase 2: drain station's balance; balance probe must auto-disable it
	stationQuota.Store(0)
	deadline := time.Now().Add(3 * time.Second)
	var stUp, diUp *strategy.Upstream
	for _, u := range ups {
		if u.Name == "st-01" {
			stUp = u
		} else {
			diUp = u
		}
	}
	for time.Now().Before(deadline) {
		if stUp.Circuit.Status() == strategy.StatusAutoDisabled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stUp.Circuit.Status() != strategy.StatusAutoDisabled {
		t.Fatalf("station must be auto-disabled after balance drained, status=%s", stUp.Circuit.Status())
	}

	code, body = chat()
	if code != 200 || !strings.Contains(body, "from-direct") {
		t.Fatalf("phase2: traffic must fail over to direct, got %d %s", code, body)
	}

	// phase 3: admin visibility — event recorded, upstreams endpoint reflects state
	req, _ := http.NewRequest(http.MethodGet, adminSrv.URL+"/admin/upstreams", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "auto_disabled") {
		t.Fatalf("admin upstreams must show auto_disabled: %s", b)
	}

	req, _ = http.NewRequest(http.MethodGet, adminSrv.URL+"/admin/events", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, _ = http.DefaultClient.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "balance below threshold") {
		t.Fatalf("events must record disable reason: %s", b)
	}

	// phase 4: manual disable of direct forces 503
	req, _ = http.NewRequest(http.MethodPost, adminSrv.URL+"/admin/upstreams/di-01/disable", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("manual disable: %d", resp.StatusCode)
	}
	code, _ = chat()
	if code != 503 {
		t.Fatalf("want 503 when all exhausted, got %d", code)
	}

	// manual enable restores
	req, _ = http.NewRequest(http.MethodPost, adminSrv.URL+"/admin/upstreams/di-01/enable", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	code, body = chat()
	if code != 200 || !strings.Contains(body, "from-direct") {
		t.Fatalf("manual enable must restore traffic, got %d %s", code, body)
	}

	_ = diUp
}
