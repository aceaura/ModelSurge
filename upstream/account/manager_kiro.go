// manager_kiro.go kiro 账号的调度运行时：AuthService + 控制面 client +
// 动态模型缓存 + 四层模型解析。构造无网络操作（凭据读取均为本地），
// 模型缓存懒预热（后台 goroutine，不阻塞请求路径）。
package account

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/aceaura/ModelSurge/upstream/proto/kiro"
)

// KiroRuntime 单个 kiro 账号的运行时组件集合。
type KiroRuntime struct {
	Auth     *AuthService
	Client   *KiroClient
	Models   *ModelInfoCache
	resolver *kiro.ModelResolver

	warmMu      sync.Mutex
	warmRunning bool
}

// Resolve canonical 模型名 -> Kiro 内部 ID（四层解析：别名->规范化->
// 缓存->隐藏->直通）。网关不是守门人：未知模型直通，由 Kiro 终裁。
func (r *KiroRuntime) Resolve(model string) (string, bool) {
	if r.resolver == nil {
		return model, true // 运行时未装配（理论不可达）：透传
	}
	return r.resolver.Resolve(model).InternalID, true
}

// AvailableModels 展示模型列表（缓存 ∪ 隐藏模型 ∪ 别名 - 隐藏项），
// /admin/models 与 /v1/models 并集口径用。
func (r *KiroRuntime) AvailableModels() []string {
	if r.resolver == nil {
		return nil
	}
	return r.resolver.AvailableModels()
}

// warmModelsAsync 后台预热模型缓存：过期才拉取，单飞（并发请求只触发一次）。
func (r *KiroRuntime) warmModelsAsync() {
	if !r.Models.Stale() {
		return
	}
	r.warmMu.Lock()
	if r.warmRunning {
		r.warmMu.Unlock()
		return
	}
	r.warmRunning = true
	r.warmMu.Unlock()

	go func() {
		defer func() {
			r.warmMu.Lock()
			r.warmRunning = false
			r.warmMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r.Models.EnsureFresh(ctx)
		log.Printf("kiro models: cache warmed (%d models)", len(r.Models.AllModelIDs()))
	}()
}

// ensureKiroRuntime 构造（或凭据变更后重建）kiro 账号运行时并注入
// ResolveModel 钩子（须持 Manager.mu；无网络操作）。
// 账号未配 region 时用 Manager 的全局默认（kiro 段 region）填充。
func (m *Manager) ensureKiroRuntime(a *Account) {
	if a.Kiro == nil {
		log.Printf("account: kiro account %s has no credentials, serving passthrough only", a.Name)
		return
	}
	k := a.Kiro
	if k.Region == "" && m.kiroRegion != "" {
		k2 := *k // 不改账号本体：全局默认只作用于运行时
		k2.Region = m.kiroRegion
		k = &k2
	}
	auth := NewAuthService(m.store, a.Name, k)
	client := NewKiroClient(auth)
	models := NewModelInfoCache(func(ctx context.Context) ([]KiroModel, error) {
		return client.ListAvailableModels(ctx)
	})
	models.SetTTL(m.kiroCacheTTL)
	auth.SetProfileFetcher(func(ctx context.Context) (string, error) {
		return client.ListAvailableProfiles(ctx)
	})
	rt := &KiroRuntime{Auth: auth, Client: client, Models: models}
	rt.resolver = kiro.NewModelResolver(models, nil, nil, nil)
	m.kiro[a.Name] = rt
	a.ResolveModel = rt.Resolve
}

// QuotaCooldownUntil 402 配额超限后的冷却时刻：GetUsageLimits 的
// usageBreakdownList[].resetDate（日期粒度，取当日结束防时区偏差）。
// 不可得（无 profileArn 的免费账号 / 拉取失败 / 无日期字段）返回零值，
// 由调用方兜底。
func (r *KiroRuntime) QuotaCooldownUntil(ctx context.Context) time.Time {
	raw, err := r.Client.GetUsageLimits(ctx)
	if err != nil {
		log.Printf("kiro quota: get usage limits for %s failed: %v", r.Auth.name, err)
		return time.Time{}
	}
	var resp struct {
		UsageBreakdownList []struct {
			ResetDate string `json:"resetDate"`
		} `json:"usageBreakdownList"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return time.Time{}
	}
	for _, u := range resp.UsageBreakdownList {
		if d, err := time.ParseInLocation("2006-01-02", u.ResetDate, time.UTC); err == nil {
			return d.Add(24 * time.Hour) // 重置日全天结束，宁冷勿争
		}
	}
	return time.Time{}
}
