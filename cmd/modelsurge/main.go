// cmd/modelsurge 模式一单进程启动入口（纯编排壳，见 design/deployment-modes.md）。
// 单份 YAML 三 section；upstream → replay → agent 顺序拉起，回环 HTTP 互联；
// 关闭反向：agent 停收流量 → outbox flush → replay → upstream。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aceaura/ModelSurge/agent/agentstore"
	"github.com/aceaura/ModelSurge/agent/relay"
	"github.com/aceaura/ModelSurge/agent/replayclient"
	agentserver "github.com/aceaura/ModelSurge/agent/server"
	"github.com/aceaura/ModelSurge/replay/bootstrap"
	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/replay/service"
	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/adminapi"
	upbootstrap "github.com/aceaura/ModelSurge/upstream/bootstrap"
	upservice "github.com/aceaura/ModelSurge/upstream/service"
	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

const (
	upstreamLoopback = "127.0.0.1:18100"
	replayLoopback   = "127.0.0.1:18101"
)

func main() {
	path := flag.String("config", "modelsurge.yaml", "modelsurge single-process config")
	flag.Parse()
	cfg, err := LoadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── upstream（最先启动）────────────────────────────────
	uStore, err := upstreamstore.Open(cfg.Upstream.DBPath)
	if err != nil {
		log.Fatalf("upstream store: %v", err)
	}
	defer uStore.Close()
	if cfg.Upstream.LegacyDB != "" {
		if err := uStore.ImportLegacy(ctx, cfg.Upstream.LegacyDB); err != nil {
			log.Fatalf("legacy import: %v", err)
		}
	}
	if err := uStore.MaterializeAccounts(); err != nil {
		log.Fatalf("materialize accounts: %v", err)
	}
	cds, err := cfg.Upstream.CooldownDurations()
	if err != nil {
		log.Fatal(err)
	}
	uMgr, err := account.NewManager(uStore.Accounts, account.Cooldowns{Default: cds.Default, Window7h: cds.Window7h, Monthly: cds.Monthly}, upbootstrap.BuildKiroDeps(cfg.Upstream.Kiro))
	if err != nil {
		log.Fatal(err)
	}
	uSvc := upservice.NewService(uStore, uMgr)
	uSvc.AccessLogEnabled = cfg.Upstream.AccessLogEnabled
	firstTokenTimeout := 30 * time.Second
	if cfg.Upstream.Kiro != nil {
		if cfg.Upstream.Kiro.FirstTokenTimeout != "" {
			firstTokenTimeout = cfg.Upstream.Kiro.FirstTokenTimeoutDur
		}
		uSvc.KiroStreamingReadTimeout = cfg.Upstream.Kiro.StreamingReadTimeoutDur
		uSvc.KiroWebSearchInject = cfg.Upstream.Kiro.WebSearchInject
	}
	uSvc.KiroFirstTokenTimeout = firstTokenTimeout
	uHTTP := upservice.NewHTTPServer(uSvc, cfg.Upstream.ServiceKey)
	uHTTP.AccessLogEnabled = cfg.Upstream.AccessLogEnabled
	if cfg.Upstream.AdminKey != "" {
		uHTTP.AdminHandler = adminapi.NewAccountAdmin(uMgr, cfg.Upstream.AdminKey, uStore.MaterializeAccounts)
	}
	uServer := &http.Server{Addr: cfg.Upstream.Listen, Handler: uHTTP.Handler(), ReadHeaderTimeout: 30 * time.Second}
	if err := serve("upstream", uServer); err != nil {
		log.Fatal(err)
	}
	if err := waitHealthy(ctx, fmt.Sprintf("http://%s/internal/v1/health", cfg.Upstream.Listen), cfg.Upstream.ServiceKey); err != nil {
		log.Fatalf("upstream health: %v", err)
	}

	// ── replay ─────────────────────────────────────────────
	rStore, err := relaystore.Open(cfg.Replay.DBPath)
	if err != nil {
		log.Fatalf("replay store: %v", err)
	}
	defer rStore.Close()
	upClient := bootstrap.NewUpstreamClient(fmt.Sprintf("http://%s", cfg.Upstream.Listen), cfg.Replay.UpstreamServiceKey, cfg.Replay.UpstreamTimeoutDuration, cfg.Replay.AccessLogEnabled)
	bootstrap.CompatBootstrap(rStore, upClient, cfg.Replay.BootstrapClientKey, cfg.Replay.UpstreamTimeoutDuration)
	_, rSvc := bootstrap.BuildService(rStore, upClient, cfg.Replay.CacheTTLDuration, cfg.Replay.AccessLogEnabled)
	rHTTP := service.NewHTTPServer(rSvc, cfg.Replay.AgentServiceKey, cfg.Replay.AdminKey)
	rHTTP.SetAccessLog(cfg.Replay.AccessLogEnabled)
	rServer := &http.Server{Addr: cfg.Replay.Listen, Handler: rHTTP.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := serve("replay", rServer); err != nil {
		log.Fatal(err)
	}
	if err := waitHealthy(ctx, fmt.Sprintf("http://%s/internal/v1/health", cfg.Replay.Listen), cfg.Replay.AgentServiceKey); err != nil {
		log.Fatalf("replay health: %v", err)
	}

	// ── agent（最后启动，对外收流量）───────────────────────
	aStore, err := agentstore.Open(cfg.Agent.DBPath)
	if err != nil {
		log.Fatalf("agent store: %v", err)
	}
	defer aStore.Close()
	rpClient := replayclient.New(cfg.Agent.ReplayURL, cfg.Agent.ServiceKey, cfg.Agent.ControlTimeoutDur)
	aServer := &http.Server{Addr: cfg.Agent.Listen, Handler: agentserver.New(&cfg.Agent, rpClient, aStore).Handler(), ReadHeaderTimeout: 30 * time.Second}
	go relay.RunOutboxWorker(ctx, aStore, rpClient, time.Second)
	if err := serve("agent", aServer); err != nil {
		log.Fatal(err)
	}
	log.Printf("modelsurge single-process ready: agent=%s replay=%s upstream=%s", cfg.Agent.Listen, cfg.Replay.Listen, cfg.Upstream.Listen)

	// ── 反向关闭：agent → outbox flush → replay → upstream ──
	<-ctx.Done()
	shutdown(aServer, "agent", 15*time.Second)
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	relay.ReplayOutbox(flushCtx, aStore, rpClient)
	cancelFlush()
	shutdown(rServer, "replay", 10*time.Second)
	shutdown(uServer, "upstream", 10*time.Second)
}

// serve 绑定端口并在 goroutine 中 Serve，绑定失败（如端口占用）同步返回。
func serve(name string, s *http.Server) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("%s listen %s: %w", name, s.Addr, err)
	}
	go func() {
		if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("%s serve: %v", name, err)
		}
	}()
	return nil
}

// waitHealthy 轮询内部 health 端点直至就绪（带 Bearer service key）。
func waitHealthy(ctx context.Context, url, key string) error {
	deadline := time.Now().Add(15 * time.Second)
	cl := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := cl.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting %s", url)
}

func shutdown(s *http.Server, name string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		log.Printf("%s shutdown: %v", name, err)
	}
}
