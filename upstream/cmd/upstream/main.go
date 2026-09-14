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

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/adminapi"
	"github.com/aceaura/ModelSurge/upstream/bootstrap"
	"github.com/aceaura/ModelSurge/upstream/processconfig"
	"github.com/aceaura/ModelSurge/upstream/redisx"
	service "github.com/aceaura/ModelSurge/upstream/service"
	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

func main() {
	path := flag.String("config", "upstream.yaml", "upstream config")
	flag.Parse()
	cfg, err := processconfig.LoadUpstream(*path)
	if err != nil {
		log.Fatal(err)
	}
	store, err := upstreamstore.Open(cfg.DBDriver, cfg.DBDSN)
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
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{Default: cds.Default, Window7h: cds.Window7h, Monthly: cds.Monthly}, bootstrap.BuildKiroDeps(cfg.Kiro))
	if err != nil {
		log.Fatal(err)
	}
	svc := service.NewService(store, mgr)
	svc.AccessLogEnabled = cfg.AccessLogEnabled
	if cfg.Redis.Addr != "" {
		svc.Redis = redisx.New(cfg.Redis)
		defer svc.Redis.Close()
	}
	firstTokenTimeout := 30 * time.Second
	if cfg.Kiro != nil {
		if cfg.Kiro.FirstTokenTimeout != "" {
			firstTokenTimeout = cfg.Kiro.FirstTokenTimeoutDur
		}
		svc.KiroStreamingReadTimeout = cfg.Kiro.StreamingReadTimeoutDur
		svc.KiroWebSearchInject = cfg.Kiro.WebSearchInject
	}
	svc.KiroFirstTokenTimeout = firstTokenTimeout
	h := service.NewHTTPServer(svc, cfg.ServiceKey)
	h.AccessLogEnabled = cfg.AccessLogEnabled
	if cfg.AdminKey != "" {
		h.AdminHandler = adminapi.NewAccountAdmin(mgr, cfg.AdminKey, store.MaterializeAccounts)
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
