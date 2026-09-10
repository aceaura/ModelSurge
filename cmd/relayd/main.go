package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"relayd/backend/admin"
	"relayd/backend/catalog"
	"relayd/backend/egress"
	"relayd/backend/ingress"
	"relayd/backend/obs"
	"relayd/backend/probe"
	"relayd/strategy"
)

func main() {
	configPath := flag.String("config", "relayd.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := catalog.Load(*configPath)
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	ring := obs.NewRing(1024)
	onTransition := func(name string, d strategy.Decision) {
		ring.Add(obs.Event{Upstream: name, From: d.From.String(), To: d.To.String(), Reason: d.Reason})
		logger.Info("upstream_state", "name", name, "from", d.From.String(), "to", d.To.String(), "reason", d.Reason)
	}

	cat := catalog.NewCatalog(cfg.Models)
	upstreams, err := cfg.BuildUpstreams(cat, onTransition)
	if err != nil {
		logger.Error("build upstreams failed", "error", err)
		os.Exit(1)
	}

	reg := strategy.NewRegistry()
	reg.Replace(upstreams)

	tokens := map[string]bool{}
	for _, t := range cfg.AuthTokens {
		tokens[t] = true
	}

	sched := probe.NewScheduler(probe.SchedulerConfig{
		Concurrency:     cfg.ProbeSchedule.Concurrency,
		Jitter:          cfg.ProbeSchedule.Jitter,
		BudgetPerMinute: cfg.ProbeSchedule.BudgetPerMinute,
		Mode:            cfg.ProbeSchedule.Mode,
	})
	probersByUpstream := map[string][]probe.Prober{}
	for _, u := range upstreams {
		var probers []probe.Prober
		for _, pc := range cfg.ProbesFor(u.Name) {
			p, err := probe.NewProber(u.Name, pc)
			if err != nil {
				logger.Error("probe config invalid", "upstream", u.Name, "error", err)
				os.Exit(1)
			}
			probers = append(probers, p)
		}
		probersByUpstream[u.Name] = probers
		sched.Register(u, probers)
	}

	relayHandler := &ingress.Handler{
		Reg:     reg,
		Catalog: cat,
		Fwd:     egress.NewForwarderWithTimeout(cfg.RequestTimeout.D()),
		Tokens:  tokens,
		MaxBody: int64(cfg.MaxBufferedBody),
		Logger:  logger,
	}

	adminSrv := &admin.Server{
		Reg:    reg,
		Ring:   ring,
		Tokens: tokens,
		ProberOf: func(name string) []probe.Prober {
			return probersByUpstream[name]
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go sched.Run(ctx)

	biz := &http.Server{Addr: cfg.Listen, Handler: relayHandler.Mux(), ReadHeaderTimeout: 30 * time.Second}
	adm := &http.Server{Addr: cfg.AdminListen, Handler: adminSrv.Mux(), ReadHeaderTimeout: 30 * time.Second}

	go func() {
		logger.Info("relay listening", "addr", cfg.Listen)
		if err := biz.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("relay server failed", "error", err)
			os.Exit(1)
		}
	}()
	go func() {
		logger.Info("admin listening", "addr", cfg.AdminListen)
		if err := adm.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("admin server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	biz.Shutdown(shutdownCtx)
	adm.Shutdown(shutdownCtx)
}
