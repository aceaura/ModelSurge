package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"relayd/backend/account"
	"relayd/backend/processconfig"
	"relayd/backend/proto/kiro"
	"relayd/backend/server"
	"relayd/backend/upstream"
	"relayd/backend/upstreamstore"
)

func kiroSetup(cfg processconfig.Upstream) account.ManagerDeps {
	if cfg.Kiro == nil {
		return account.ManagerDeps{}
	}
	k := cfg.Kiro
	if k.Cloud != nil && k.Cloud.Enabled {
		account.SetCloudConfig(account.CloudConfig{ForwardURL: k.Cloud.ForwardURL, APIKey: k.Cloud.APIKey})
	}
	account.SetKiroDebug(k.Debug, k.DebugDir)
	kiro.SetOptions(kiro.Options{FakeReasoning: k.FakeReasoning, FakeReasoningMaxTokens: k.FakeReasoningMaxTokens, TruncationRecovery: true})
	return account.ManagerDeps{BreakerBase: k.RecoveryTimeoutDur, BreakerMax: k.RecoveryTimeoutDur * time.Duration(k.MaxBackoffMultiplier), ProbeRate: k.ProbabilisticRetry, KiroRegion: k.Region, KiroCacheTTL: k.CacheTTLDur}
}

func main() {
	path := flag.String("config", "upstream.yaml", "upstream config")
	flag.Parse()
	cfg, err := processconfig.LoadUpstream(*path)
	if err != nil {
		log.Fatal(err)
	}
	store, err := upstreamstore.Open(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	if cfg.LegacyDB != "" {
		if err := store.ImportLegacy(context.Background(), cfg.LegacyDB); err != nil {
			log.Fatalf("legacy import: %v", err)
		}
	}
	if err := store.MaterializeAccounts(); err != nil {
		log.Fatal(err)
	}
	cds, err := cfg.CooldownDurations()
	if err != nil {
		log.Fatal(err)
	}
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{Default: cds.Default, Window7h: cds.Window7h, Monthly: cds.Monthly}, kiroSetup(cfg))
	if err != nil {
		log.Fatal(err)
	}
	svc := upstream.NewService(store, mgr)
	h := upstream.NewHTTPServer(svc, cfg.ServiceKey)
	if cfg.AdminKey != "" {
		h.AdminHandler = server.NewAccountAdmin(mgr, cfg.AdminKey, store.MaterializeAccounts)
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: h.Handler(), ReadHeaderTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("upstream listening on %s", cfg.Listen)
	if err := serve(srv, ctx); err != nil {
		log.Fatal(err)
	}
}
func serve(s *http.Server, ctx context.Context) error {
	ch := make(chan error, 1)
	go func() {
		err := s.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		ch <- err
	}()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = s.Shutdown(c)
		return <-ch
	}
}
