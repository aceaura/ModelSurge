package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aceaura/ModelSurge/replay/bootstrap"
	"github.com/aceaura/ModelSurge/replay/config"
	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/replay/service"
)

func main() {
	configPath := flag.String("config", "replay.yaml", "path to replay configuration")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	store, err := relaystore.Open(cfg.DBDriver, cfg.DBDSN)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	upstream := bootstrap.NewUpstreamClient(cfg.UpstreamURL, cfg.UpstreamServiceKey, cfg.UpstreamTimeoutDuration, cfg.AccessLogEnabled)
	bootstrap.CompatBootstrap(store, upstream, cfg.BootstrapClientKey, cfg.UpstreamTimeoutDuration)

	_, app := bootstrap.BuildService(store, upstream, cfg.CacheTTLDuration, cfg.AccessLogEnabled)
	handler := service.NewHTTPServer(app, cfg.AgentServiceKey, cfg.AdminKey)
	handler.SetAccessLog(cfg.AccessLogEnabled)
	httpServer := &http.Server{Addr: cfg.Listen, Handler: handler.Handler(), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Printf("replay listening on %s", cfg.Listen)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("replay shutdown: %v", err)
	}
}
