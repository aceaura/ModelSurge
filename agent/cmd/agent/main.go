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

	"github.com/aceaura/ModelSurge/agent/agentstore"
	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/proto/kiro"
	"github.com/aceaura/ModelSurge/agent/relay"
	"github.com/aceaura/ModelSurge/agent/replayclient"
	"github.com/aceaura/ModelSurge/agent/server"
)

func main() {
	path := flag.String("config", "agent.yaml", "agent config")
	flag.Parse()
	cfg, err := config.Load(*path)
	if err != nil {
		log.Fatal(err)
	}
	if k := cfg.Kiro; k != nil {
		kiro.SetOptions(kiro.Options{
			FakeReasoning: k.FakeReasoning, FakeReasoningMaxTokens: k.FakeReasoningMaxTokens,
			FakeReasoningBudgetCap: k.FakeReasoningBudgetCap, TruncationRecovery: cfg.TruncationRecoveryEnabled,
		})
	}
	store, err := agentstore.Open(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	replay := replayclient.New(cfg.ReplayURL, cfg.ServiceKey, cfg.ControlTimeoutDur)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ControlTimeoutDur)
	if _, err := replay.Health(ctx); err != nil {
		log.Printf("agent: replay unavailable at startup: %v", err)
	}
	cancel()
	srv := &http.Server{Addr: cfg.Listen, Handler: server.New(cfg, replay, store).Handler(), ReadHeaderTimeout: 30 * time.Second}
	sig, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go relay.RunOutboxWorker(sig, store, replay, time.Second)
	log.Printf("agent listening on %s", cfg.Listen)
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
