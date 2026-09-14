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

	"github.com/aceaura/ModelSurge/replay/config"
	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/replay/schedule"
	"github.com/aceaura/ModelSurge/replay/service"
	"github.com/aceaura/ModelSurge/replay/upstreamclient"
)

func main() {
	configPath := flag.String("config", "replay.yaml", "path to replay configuration")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	store, err := relaystore.Open(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	upstream := upstreamclient.New(cfg.UpstreamURL, cfg.UpstreamServiceKey, cfg.UpstreamTimeoutDuration)
	upstream.AccessLogEnabled = cfg.AccessLogEnabled
	if cfg.BootstrapClientKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.UpstreamTimeoutDuration)
		models, bootstrapErr := upstream.Models(ctx)
		cancel()
		if bootstrapErr != nil {
			log.Printf("compatibility bootstrap skipped: %v", bootstrapErr)
		} else {
			ids := make([]string, 0, len(models))
			for _, model := range models {
				ids = append(ids, model.ID)
			}
			if err := store.Bootstrap(context.Background(), cfg.BootstrapClientKey, ids); err != nil {
				log.Fatal(err)
			}
			for _, id := range ids {
				if err := store.AddMembers(context.Background(), "compat/"+id, []string{id}); err != nil {
					log.Fatal(err)
				}
			}
		}
	}

	scheduler := &schedule.Scheduler{Store: store, Upstream: upstream, CacheTTL: cfg.CacheTTLDuration, AccessLogEnabled: cfg.AccessLogEnabled, AccessLogConfigured: true}
	app := &service.Service{Store: store, Scheduler: scheduler, Upstream: upstream, AccessLogEnabled: cfg.AccessLogEnabled, AccessLogConfigured: true}
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
