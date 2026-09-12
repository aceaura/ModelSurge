// relayd：四通八达的 LLM 协议转换网关。
// 客户端可用 Anthropic / OpenAI Chat / OpenAI Responses / Gemini 任一协议接入，
// 上游可以是其中任一协议，转换经由统一 IR 中转。
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
	"relayd/backend/config"
	"relayd/backend/proto"
	"relayd/backend/proto/kiro"
	"relayd/backend/server"
)

// shutdownGrace 等待在途请求（含流式响应）完成的上限。
const shutdownGrace = 15 * time.Second

// kiroDeps kiro 段 -> 调度器可选依赖（进程级装配，须先于任何 Kiro transport）。
func kiroSetup(cfg *config.Config) account.ManagerDeps {
	if cfg.Kiro == nil {
		return account.ManagerDeps{}
	}
	k := cfg.Kiro
	if k.Cloud != nil && k.Cloud.Enabled {
		account.SetCloudConfig(account.CloudConfig{ForwardURL: k.Cloud.ForwardURL, APIKey: k.Cloud.APIKey})
	}
	account.SetKiroDebug(k.Debug, k.DebugDir)
	// truncation_recovery 沿用顶层开关（单一来源，relay 截断追踪与 kiro 系统提示一致）。
	kiro.SetOptions(kiro.Options{
		FakeReasoning:          k.FakeReasoning,
		FakeReasoningMaxTokens: k.FakeReasoningMaxTokens,
		TruncationRecovery:     cfg.TruncationRecoveryEnabled,
	})
	max := k.RecoveryTimeoutDur * time.Duration(k.MaxBackoffMultiplier)
	return account.ManagerDeps{
		BreakerBase:  k.RecoveryTimeoutDur,
		BreakerMax:   max,
		ProbeRate:    k.ProbabilisticRetry,
		KiroRegion:   k.Region,
		KiroCacheTTL: k.CacheTTLDur,
	}
}

// kiroSeedFrom config.Upstream(kiro) -> account 种子（凭据按优先级取源）。
func kiroSeedFrom(u config.Upstream) account.SeedKiro {
	s := account.KiroAccount{
		APIRegion:     u.Kiro.APIRegion,
		ProfileArn:    u.Kiro.ProfileArn,
		WebSearch:     u.Kiro.WebSearch,
		FakeReasoning: u.Kiro.FakeReasoning,
		Region:        u.Kiro.Region,
	}
	switch {
	case u.Kiro.RefreshToken != "":
		s.Source, s.RefreshToken = account.SourceRefreshToken, u.Kiro.RefreshToken
	case u.Kiro.CredsFile != "":
		s.Source, s.CredsFile = account.SourceCredsFile, u.Kiro.CredsFile
	default:
		s.Source, s.CliDB = account.SourceCliDB, u.Kiro.CliDB
	}
	return account.SeedKiro{Name: u.Name, Kiro: s}
}

func main() {
	cfgPath := flag.String("config", "relayd.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	deps := kiroSetup(cfg) // kiro 全局装配（云中转/调试/思考注入），须先于 scheduler

	var sched *account.Manager
	if cfg.Scheduler != nil {
		store, err := account.Open(cfg.Scheduler.DBPath)
		if err != nil {
			log.Fatalf("open scheduler db: %v", err)
		}
		defer store.Close()
		var seeds []account.SeedUpstream
		var kiroSeeds []account.SeedKiro
		for _, u := range cfg.Upstreams {
			if u.Protocol == "kiro" {
				kiroSeeds = append(kiroSeeds, kiroSeedFrom(u))
				continue
			}
			seeds = append(seeds, account.SeedUpstream{
				Name: u.Name, Protocol: u.Protocol, BaseURL: u.BaseURL,
				APIKey: u.APIKey, Models: u.Models, Overrides: u.RequestOverrides,
			})
		}
		cd := cfg.Scheduler.CooldownsDur
		sched, err = account.NewManager(store, seeds, kiroSeeds, account.Cooldowns{
			Default: cd.Default, Window7h: cd.Window7h, Monthly: cd.Monthly,
		}, deps)
		if err != nil {
			log.Fatalf("init scheduler: %v", err)
		}
		for _, a := range sched.Status() {
			state := "ready"
			if a.Disabled {
				state = "disabled"
			} else if a.CooldownUntil.After(time.Now()) {
				state = "cooling until " + a.CooldownUntil.Format("15:04:05")
			}
			kind := a.Protocol
			if a.Type == account.TypeKiro {
				kind = "kiro" + accountLabel(a)
			}
			log.Printf("relayd: account %s (%s) %s", a.Name, kind, state)
		}
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg, sched).Handler(),
		ReadHeaderTimeout: 30 * time.Second, // 防 slowloris 慢速连接占资源
	}
	log.Printf("relayd listening on %s, protocols: %v, upstreams: %d", cfg.Listen, proto.Names(), len(cfg.Upstreams))

	// 优雅停机：SIGINT/SIGTERM 后停止收新请求，
	// 等在途请求（含流式响应）完成或超时强制断开。
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(srv, sigCtx); err != nil {
		log.Fatal(err)
	}
}

// accountLabel kiro 账号附加凭据源标注（启动日志区分账号形态）。
func accountLabel(a account.Account) string {
	if a.Kiro == nil || a.Kiro.Source == "" {
		return ""
	}
	return "/" + a.Kiro.Source
}

// serve 启动 HTTP 服务并阻塞：sigCtx 取消（收到信号）后优雅停机；
// ListenAndServe 自身出错（如端口占用）立即返回。
func serve(srv *http.Server, sigCtx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case err := <-errCh:
		return err
	case <-sigCtx.Done():
		log.Printf("relayd shutting down, waiting up to %s for in-flight requests", shutdownGrace)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("relayd: forced shutdown: %v", err)
		}
		<-errCh
		log.Printf("relayd stopped")
		return nil
	}
}
