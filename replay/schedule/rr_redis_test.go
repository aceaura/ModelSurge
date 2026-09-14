package schedule

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

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

// 门控：双副本（两个 Scheduler 共享 Redis 键空间）round_robin
// 收敛为全局序（设计 2.4/需求 2.4）。
func TestRoundRobinGlobalCursorRedis(t *testing.T) {
	prefix := "modelsurge-rr-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	c1 := redisx.New(redisx.Config{Addr: testRedisAddr(t), Prefix: prefix})
	c2 := redisx.New(redisx.Config{Addr: testRedisAddr(t), Prefix: prefix})
	defer c1.Close()
	defer c2.Close()
	if !c1.Available() || !c2.Available() {
		t.Fatalf("redis %s not reachable", testRedisAddr(t))
	}
	s1 := &Scheduler{Redis: c1}
	s2 := &Scheduler{Redis: c2}
	ctx := context.Background()
	group := "g-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	// 两副本交替取游标：全局序 0,1,2,0,1,2（n=3）
	for i := 0; i < 6; i++ {
		s := s1
		if i%2 == 1 {
			s = s2
		}
		if got := s.rrCursor(ctx, group, 3); got != i%3 {
			t.Fatalf("cursor %d = %d, want %d (global sequence broken)", i, got, i%3)
		}
	}
}

// 降级：Redis 配置但不可达 → 副本内局部轮询（设计 2.5 明示可接受）。
func TestRoundRobinDegradedLocalFallback(t *testing.T) {
	c := redisx.New(redisx.Config{Addr: "127.0.0.1:1"})
	defer c.Close()
	s := &Scheduler{Redis: c}
	ctx := context.Background()
	if got := s.rrCursor(ctx, "g", 3); got != 0 {
		t.Fatalf("first local cursor = %d", got)
	}
	if got := s.rrCursor(ctx, "g", 3); got != 1 {
		t.Fatalf("second local cursor = %d", got)
	}
}

// 未配置（模式一）：恒局部轮询，现行为不变。
func TestRoundRobinNilRedisLocal(t *testing.T) {
	s := &Scheduler{}
	ctx := context.Background()
	if got := s.rrCursor(ctx, "g", 2); got != 0 {
		t.Fatalf("first = %d", got)
	}
	if got := s.rrCursor(ctx, "g", 2); got != 1 {
		t.Fatalf("second = %d", got)
	}
	if got := s.rrCursor(ctx, "g", 2); got != 0 {
		t.Fatalf("wrap = %d", got)
	}
}
