// Package bootstrap 承载 Replay 进程装配依赖：从 cmd 的 main 下沉为导出
// 函数，供 replay/cmd/replay 与根模块 cmd/modelsurge 共用。
package bootstrap

import (
	"context"
	"log"
	"time"

	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/replay/schedule"
	"github.com/aceaura/ModelSurge/replay/service"
	"github.com/aceaura/ModelSurge/replay/upstreamclient"
)

// CompatBootstrap 在配置了 bootstrap_client_key 时，拉取 Upstream 模型清单并
// 为每个模型建立 user_model、compat 组与成员（幂等，仅建缺失项）。
// Upstream 不可用时跳过并记录日志，不阻断启动。
func CompatBootstrap(store *relaystore.Store, upstream *upstreamclient.Client, clientKey string, timeout time.Duration) {
	if clientKey == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	models, err := upstream.Models(ctx)
	cancel()
	if err != nil {
		log.Printf("compatibility bootstrap skipped: %v", err)
		return
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	if err := store.Bootstrap(context.Background(), clientKey, ids); err != nil {
		log.Fatal(err)
	}
	for _, id := range ids {
		if err := store.AddMembers(context.Background(), "compat/"+id, []string{id}); err != nil {
			log.Fatal(err)
		}
	}
}

// NewUpstreamClient 构造带访问日志开关的 Upstream 客户端。
func NewUpstreamClient(url, serviceKey string, timeout time.Duration, accessLog bool) *upstreamclient.Client {
	c := upstreamclient.New(url, serviceKey, timeout)
	c.AccessLogEnabled = accessLog
	return c
}

// BuildService 组装 Scheduler 与 Service（三进程入口与单进程入口共用）。
// Redis 热态经 store.Redis 派生：两处使用方（鉴权缓存/rr 游标）共享同一客户端。
func BuildService(store *relaystore.Store, upstream *upstreamclient.Client, cacheTTL time.Duration, accessLog bool) (*schedule.Scheduler, *service.Service) {
	scheduler := &schedule.Scheduler{Store: store, Upstream: upstream, CacheTTL: cacheTTL, AccessLogEnabled: accessLog, AccessLogConfigured: true, Redis: store.Redis}
	app := &service.Service{Store: store, Scheduler: scheduler, Upstream: upstream, AccessLogEnabled: accessLog, AccessLogConfigured: true}
	return scheduler, app
}
