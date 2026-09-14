// Package bootstrap 承载 Upstream 进程装配依赖：从 cmd 的 main 下沉为
// 导出函数，供 upstream/cmd/upstream 与根模块 cmd/modelsurge 共用。
package bootstrap

import (
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/config"
	"github.com/aceaura/ModelSurge/upstream/proto/kiro"
)

// BuildKiroDeps 把 upstream.yaml 的 kiro section 装配为 account.ManagerDeps，
// 并应用 cloud/debug/fake_reasoning 全局开关。cfg.Kiro 为 nil 时返回零值。
func BuildKiroDeps(cfg *config.Kiro) account.ManagerDeps {
	if cfg == nil {
		return account.ManagerDeps{}
	}
	k := cfg
	if k.Cloud != nil && k.Cloud.Enabled {
		account.SetCloudConfig(account.CloudConfig{ForwardURL: k.Cloud.ForwardURL, APIKey: k.Cloud.APIKey})
	}
	account.SetKiroDebug(k.Debug, k.DebugDir)
	kiro.SetOptions(kiro.Options{FakeReasoning: k.FakeReasoning, FakeReasoningMaxTokens: k.FakeReasoningMaxTokens, FakeReasoningBudgetCap: k.FakeReasoningBudgetCap, TruncationRecovery: true})
	return account.ManagerDeps{BreakerBase: k.RecoveryTimeoutDur, BreakerMax: k.RecoveryTimeoutDur * time.Duration(k.MaxBackoffMultiplier), ProbeRate: k.ProbabilisticRetry, KiroRegion: k.Region, KiroCacheTTL: k.CacheTTLDur}
}
