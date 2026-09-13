package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/proto/kiro"
)

// 模型缓存：TTL 过期懒刷新、失败保留旧值、隐藏模型注入。
func TestModelInfoCache(t *testing.T) {
	var fetchCalls int
	var fetched []KiroModel
	fetch := func(ctx context.Context) ([]KiroModel, error) {
		fetchCalls++
		return fetched, nil
	}
	c := NewModelInfoCache(fetch)
	now := time.Now()
	c.now = func() time.Time { return now }

	// 从未更新 -> 过期 -> 懒刷新
	fetched = []KiroModel{{ModelID: "claude-sonnet-4.5"}, {ModelID: "auto"}}
	c.EnsureFresh(context.Background())
	if fetchCalls != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetchCalls)
	}
	if !c.IsValid("claude-sonnet-4.5") || !c.IsValid("auto") || c.IsValid("glm-5") {
		t.Errorf("cache contents wrong: %v", c.AllModelIDs())
	}

	// 未过期：不再刷新
	c.EnsureFresh(context.Background())
	if fetchCalls != 1 {
		t.Errorf("fetch calls = %d, want 1 (fresh cache reused)", fetchCalls)
	}

	// 隐藏模型注入：不覆盖已有
	c.AddHiddenModel("claude-3.7-sonnet", "auto")
	if !c.IsValid("claude-3.7-sonnet") {
		t.Error("hidden model not injected")
	}

	// 过 TTL：重新拉取替换（隐藏模型丢失属预期——注入方在刷新后再补）
	now = now.Add(13 * time.Hour)
	fetched = []KiroModel{{ModelID: "glm-5"}}
	c.EnsureFresh(context.Background())
	if fetchCalls != 2 || c.IsValid("claude-sonnet-4.5") || !c.IsValid("glm-5") {
		t.Errorf("after TTL refresh: calls=%d models=%v", fetchCalls, c.AllModelIDs())
	}

	// 刷新失败：保留旧数据
	now = now.Add(13 * time.Hour)
	c.fetch = func(ctx context.Context) ([]KiroModel, error) { return nil, errors.New("boom") }
	c.EnsureFresh(context.Background())
	if !c.IsValid("glm-5") {
		t.Error("stale cache must survive failed refresh")
	}
}

// 刷新失败且缓存为空：回落静态兜底表（account_manager.py FALLBACK_MODELS 语义）。
func TestModelInfoCacheFallbackSeed(t *testing.T) {
	c := NewModelInfoCache(func(ctx context.Context) ([]KiroModel, error) {
		return nil, errors.New("network down")
	})
	now := time.Now()
	c.now = func() time.Time { return now }

	c.EnsureFresh(context.Background())
	if len(c.AllModelIDs()) == 0 {
		t.Fatal("empty cache on failed refresh must be seeded with fallback models")
	}
	for _, id := range kiro.FallbackModelIDs() {
		if !c.IsValid(id) {
			t.Errorf("fallback model %q missing after seed", id)
		}
	}

	// 兜底种子后缓存非空：再次失败保留种子（直到网络恢复刷真值）
	now = now.Add(13 * time.Hour)
	c.EnsureFresh(context.Background())
	if !c.IsValid(kiro.FallbackModelIDs()[0]) {
		t.Error("fallback seed must survive subsequent failed refresh")
	}
}
