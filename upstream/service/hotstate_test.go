package upstream

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/redisx"
)

// testRedisAddr 门控：TEST_REDIS_ADDR 未设时跳过。
func testRedisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	return addr
}

// openBreakerTarget 让 kiro/* 进入熔断退避冷却（failures=1、class=error、
// 冷却 1min）。走 svc.Report（Attempt=1 避开 kiro 首试 retrying 特判）：
// 与生产路径一致，applied 后主动失效读旁路缓存。
func openBreakerTarget(t *testing.T, svc *Service, reportID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.Report(ctx, upstreamv1.ResultReport{
		ReportID: reportID, TargetID: "kiro/*", Outcome: "error", Status: 500, Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// 门控：Evaluate 读旁路 + Half-Open 全局试探收敛 + 报告写后失效（设计 2.4/2.5）。
func TestEvaluateHotStateGated(t *testing.T) {
	ctx := context.Background()
	svc, _ := testKiroService(t)
	addr := testRedisAddr(t)
	prefix := "modelsurge-state-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	svc.Redis = redisx.New(redisx.Config{Addr: addr, Prefix: prefix})
	if !svc.Redis.Available() {
		t.Fatalf("redis %s not reachable", addr)
	}
	defer svc.Redis.Close()

	target := "kiro/*"
	// 无冷却：available + 读旁路缓存回填
	evals, err := svc.Evaluate(ctx, []string{target})
	if err != nil || len(evals) != 1 || !evals[0].Available {
		t.Fatalf("fresh evaluate: %+v %v", evals, err)
	}

	// 熔断冷却（瞬态失败 → 退避 1min，failures=1，class=error）
	openBreakerTarget(t, svc, "rep-hot-1")

	// 副本 1：抢到全局试探锁 → 放行（Half-Open）
	evals, err = svc.Evaluate(ctx, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	if !evals[0].Available {
		t.Fatalf("probe not granted on first evaluate: %+v", evals[0])
	}
	// 执行侧核验：试探锁在途 → Resolve 放行（链路一致）
	if _, uerr := svc.Resolve(ctx, target); uerr != nil {
		t.Fatalf("resolve during in-flight probe: %+v", uerr)
	}

	// 副本 2（同库同键空间、独立 Redis 客户端）：锁被持有 → cooling_down
	mgr2, err := account.NewManager(svc.Store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	svc2 := NewService(svc.Store, mgr2)
	svc2.Redis = redisx.New(redisx.Config{Addr: addr, Prefix: prefix})
	defer svc2.Redis.Close()
	evals2, err := svc2.Evaluate(ctx, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	if evals2[0].Available || evals2[0].ExclusionReason != "cooling_down" {
		t.Fatalf("second replica must not probe while lock held: %+v", evals2[0])
	}

	// 恢复：Report(normal) 写 PG + 主动失效 → 两副本立即可用
	if _, err := svc.Report(ctx, upstreamv1.ResultReport{
		ReportID: "rep-hot-2", TargetID: target, Outcome: "normal",
	}); err != nil {
		t.Fatal(err)
	}
	evals, err = svc.Evaluate(ctx, []string{target})
	if err != nil || !evals[0].Available {
		t.Fatalf("after recovery (replica 1): %+v %v", evals, err)
	}
	evals2, err = svc2.Evaluate(ctx, []string{target})
	if err != nil || !evals2[0].Available {
		t.Fatalf("after recovery (replica 2): %+v %v", evals2, err)
	}
}

// 未配置 Redis（模式一）：冷却目标严格排除，无试探（现行为不变）。
func TestEvaluateNilRedisStrict(t *testing.T) {
	ctx := context.Background()
	svc, _ := testKiroService(t)
	openBreakerTarget(t, svc, "rep-strict")
	evals, err := svc.Evaluate(ctx, []string{"kiro/*"})
	if err != nil {
		t.Fatal(err)
	}
	if evals[0].Available || evals[0].ExclusionReason != "cooling_down" {
		t.Fatalf("nil redis must strict-skip: %+v", evals[0])
	}
	if _, uerr := svc.Resolve(ctx, "kiro/*"); uerr == nil {
		t.Fatal("resolve must reject cooling target without redis")
	}
}

// 降级：Redis 配置但不可达 → DB 直读（现行为）+ rand 兜底不报错（设计 2.5）。
func TestEvaluateDegradedFallback(t *testing.T) {
	ctx := context.Background()
	svc, _ := testKiroService(t)
	svc.Redis = redisx.New(redisx.Config{Addr: "127.0.0.1:1"})
	defer svc.Redis.Close()
	openBreakerTarget(t, svc, "rep-deg")
	evals, err := svc.Evaluate(ctx, []string{"kiro/*"})
	if err != nil {
		t.Fatal(err)
	}
	// rand 兜底概率性放行：只断言结果合法（available 或 cooling_down）
	if !evals[0].Available && evals[0].ExclusionReason != "cooling_down" {
		t.Fatalf("degraded evaluate: %+v", evals[0])
	}
}

// 限流/鉴权类冷却不试探（试探只浪费被拒请求）：429 → class=rate_limit。
func TestProbeSkipsRateLimitClass(t *testing.T) {
	ctx := context.Background()
	svc, _ := testKiroService(t)
	addr := testRedisAddr(t)
	prefix := "modelsurge-rl-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	svc.Redis = redisx.New(redisx.Config{Addr: addr, Prefix: prefix})
	if !svc.Redis.Available() {
		t.Fatalf("redis %s not reachable", addr)
	}
	defer svc.Redis.Close()

	if _, err := svc.Store.ApplyReport(ctx, upstreamv1.ResultReport{
		ReportID: "rep-rl", TargetID: "kiro/*", Outcome: "error", Status: 429,
	}); err != nil {
		t.Fatal(err)
	}
	evals, err := svc.Evaluate(ctx, []string{"kiro/*"})
	if err != nil {
		t.Fatal(err)
	}
	if evals[0].Available || evals[0].ExclusionReason != "cooling_down" {
		t.Fatalf("rate_limit cooldown must not probe: %+v", evals[0])
	}
}
