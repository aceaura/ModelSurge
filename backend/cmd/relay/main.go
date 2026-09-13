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

	"relayd/backend/processconfig"
	"relayd/backend/relaystore"
	"relayd/backend/schedule"
	"relayd/backend/server"
	"relayd/backend/upstreamclient"
)

func main() {
	path := flag.String("config", "relay.yaml", "relay config")
	flag.Parse()
	cfg, err := processconfig.LoadRelay(*path)
	if err != nil {
		log.Fatal(err)
	}
	store, err := relaystore.Open(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	cl := upstreamclient.New(cfg.UpstreamURL, cfg.ServiceKey, cfg.ControlTimeoutDuration())
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ControlTimeoutDuration())
	models, err := cl.Models(ctx)
	cancel()
	if err != nil {
		log.Printf("relay: upstream catalog unavailable at startup: %v", err)
	} else if cfg.Bootstrap {
		names := make([]string, 0, len(models))
		byDisplay := map[string][]string{}
		for _, m := range models {
			names = append(names, m.DisplayName)
			byDisplay[m.DisplayName] = append(byDisplay[m.DisplayName], m.ID)
		}
		if err := store.Bootstrap(context.Background(), cfg.APIKey, names); err != nil {
			log.Fatal(err)
		}
		for name, ids := range byDisplay {
			if err := store.AddMembers(context.Background(), "compat/"+name, ids); err != nil {
				log.Fatalf("bootstrap group %s: %v", name, err)
			}
		}
	}
	sched := &schedule.Scheduler{Store: store, Upstream: cl}
	srv := &http.Server{Addr: cfg.Listen, Handler: server.NewRelay(cfg.LegacyConfig(), sched).Handler(), ReadHeaderTimeout: 30 * time.Second}
	sig, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("relay listening on %s", cfg.Listen)
	if err := serve(srv, sig); err != nil {
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
