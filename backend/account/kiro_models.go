// kiro_models.go 单 kiro 账号的动态模型缓存（KiroaaS cache.py ModelInfoCache
// 的 Go 翻译）：TTL + 懒刷新 + 隐藏模型注入。
// 隐式实现 kiro.ModelCache 接口（IsValid/AllModelIDs），由 relay 装配对接。
package account

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"relayd/backend/proto/kiro"
)

// DefaultModelCacheTTL 模型缓存 TTL（spec：12h）。
const DefaultModelCacheTTL = 12 * time.Hour

// ModelInfoCache 模型元数据缓存（线程安全）。
type ModelInfoCache struct {
	mu         sync.Mutex
	models     map[string]KiroModel // modelId -> 条目
	lastUpdate time.Time
	ttl        time.Duration
	fetch      func(ctx context.Context) ([]KiroModel, error)
	now        func() time.Time
}

// NewModelInfoCache 构造。fetch 为模型拉取函数（KiroClient 注入），
// nil 则缓存永不自动刷新（仅手动 Update）。
func NewModelInfoCache(fetch func(ctx context.Context) ([]KiroModel, error)) *ModelInfoCache {
	return &ModelInfoCache{
		models: map[string]KiroModel{},
		ttl:    DefaultModelCacheTTL,
		fetch:  fetch,
		now:    time.Now,
	}
}

// SetTTL 设置缓存 TTL（config kiro.cache_ttl；非正值保持默认）。
func (c *ModelInfoCache) SetTTL(ttl time.Duration) {
	if ttl > 0 {
		c.ttl = ttl
	}
}

// Update 用新列表整体替换缓存内容。
func (c *ModelInfoCache) Update(models []KiroModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = make(map[string]KiroModel, len(models))
	for _, m := range models {
		if m.ModelID != "" {
			c.models[m.ModelID] = m
		}
	}
	c.lastUpdate = c.now()
}

// AddHiddenModel 注入隐藏模型（/ListAvailableModels 不返回但可用）。
func (c *ModelInfoCache) AddHiddenModel(displayName, internalID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.models[displayName]; ok {
		return
	}
	c.models[displayName] = KiroModel{
		ModelID:     displayName,
		ModelName:   displayName,
		Description: "Hidden model (internal: " + internalID + ")",
	}
}

// IsValid 模型是否在缓存中（kiro.ModelCache 接口）。
func (c *ModelInfoCache) IsValid(modelID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.models[modelID]
	return ok
}

// AllModelIDs 全部模型 ID（排序稳定；kiro.ModelCache 接口）。
func (c *ModelInfoCache) AllModelIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.models))
	for id := range c.models {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// MaxInputTokens 模型输入上限（缺省 DefaultMaxInputTokens）。
func (c *ModelInfoCache) MaxInputTokens(modelID string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.models[modelID]; ok && m.TokenLimits.MaxInputTokens > 0 {
		return m.TokenLimits.MaxInputTokens
	}
	return 200000
}

// Stale 缓存是否超过 TTL（从未更新视为过期）。
func (c *ModelInfoCache) Stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastUpdate.IsZero() || c.now().Sub(c.lastUpdate) > c.ttl
}

// EnsureFresh 懒刷新：过期才拉取；失败保留旧数据（下次再试），
// 缓存为空时回落静态兜底表（account_manager.py 语义：拉取耗尽用
// FALLBACK_MODELS 填充，待下轮 TTL 网络恢复再刷真值）。
func (c *ModelInfoCache) EnsureFresh(ctx context.Context) {
	if !c.Stale() {
		return
	}
	c.mu.Lock()
	fetch := c.fetch
	c.mu.Unlock()
	if fetch == nil {
		return
	}
	models, err := fetch(ctx)
	if err != nil {
		log.Printf("kiro models: refresh failed, keeping stale cache: %v", err)
		if c.isEmpty() {
			fallback := kiro.FallbackModelIDs()
			entries := make([]KiroModel, 0, len(fallback))
			for _, id := range fallback {
				entries = append(entries, KiroModel{ModelID: id, ModelName: id})
			}
			c.Update(entries)
			log.Printf("kiro models: cache empty, seeded %d fallback models", len(entries))
		}
		return
	}
	c.Update(models)
}

// isEmpty 空缓存判断（调用方持锁与否皆可，内部自锁）。
func (c *ModelInfoCache) isEmpty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.models) == 0
}
