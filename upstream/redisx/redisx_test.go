package redisx

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
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

// newTestClientWith 指定前缀的客户端（同前缀 = 共享键空间，模拟多副本）。
func newTestClientWith(t *testing.T, prefix string) *Client {
	t.Helper()
	c := New(Config{Addr: testRedisAddr(t), Prefix: prefix})
	if !c.Available() {
		t.Fatalf("redis %s not reachable", testRedisAddr(t))
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// newTestClient 唯一前缀客户端（防同实例多轮/多包测试串键）。
func newTestClient(t *testing.T) *Client {
	t.Helper()
	return newTestClientWith(t, "modelsurge-test-"+strconv.FormatInt(time.Now().UnixNano(), 10)+":")
}

func TestNormalizeDefaultPrefix(t *testing.T) {
	var c Config
	c.Normalize()
	if c.Prefix != "modelsurge:" {
		t.Fatalf("prefix = %q", c.Prefix)
	}
	c2 := Config{Prefix: "custom:"}
	c2.Normalize()
	if c2.Prefix != "custom:" {
		t.Fatal("custom prefix overwritten")
	}
}

// 降级开关：死地址 New 即降级，各 op 立即报错（不打网络、不阻塞热路径）。
func TestDegradedFastFail(t *testing.T) {
	c := New(Config{Addr: "127.0.0.1:1"})
	defer c.Close()
	if c.Available() {
		t.Fatal("client available against dead addr")
	}
	ctx := context.Background()
	if _, err := c.Incr(ctx, "k"); err == nil {
		t.Fatal("Incr succeeded while degraded")
	}
	if _, err := c.TryLock(ctx, "k", time.Second); err == nil {
		t.Fatal("TryLock succeeded while degraded")
	}
	if _, _, err := c.Get(ctx, "k"); err == nil {
		t.Fatal("Get succeeded while degraded")
	}
	if err := c.SetEx(ctx, "k", "v", time.Second); err == nil {
		t.Fatal("SetEx succeeded while degraded")
	}
	if err := c.Del(ctx, "k"); err == nil {
		t.Fatal("Del succeeded while degraded")
	}
	if err := c.Del(ctx); err != nil {
		t.Fatalf("empty Del should no-op: %v", err)
	}
}

// 门控（TEST_REDIS_ADDR）：全部 op 语义 + 跨客户端共享（多副本收敛的物理基础）。
func TestOpsGated(t *testing.T) {
	prefix := "modelsurge-test-shared-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	c := newTestClientWith(t, prefix)
	c2 := newTestClientWith(t, prefix) // 同前缀第二客户端：模拟第二副本
	ctx := context.Background()

	if v, err := c.Incr(ctx, "counter"); err != nil || v != 1 {
		t.Fatalf("incr 1: %d %v", v, err)
	}
	if v, err := c2.Incr(ctx, "counter"); err != nil || v != 2 {
		t.Fatalf("incr from second client: %d %v", v, err)
	}

	if ok, err := c.TryLock(ctx, "lock", 100*time.Millisecond); err != nil || !ok {
		t.Fatalf("first lock: %v %v", ok, err)
	}
	if ok, err := c2.TryLock(ctx, "lock", 100*time.Millisecond); err != nil || ok {
		t.Fatalf("second replica lock while held: %v %v", ok, err)
	}
	time.Sleep(150 * time.Millisecond)
	if ok, err := c2.TryLock(ctx, "lock", time.Minute); err != nil || !ok {
		t.Fatalf("re-lock after ttl expiry: %v %v", ok, err)
	}
	if ok, err := c.Exists(ctx, "lock"); err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
	if ok, err := c.Exists(ctx, "nope"); err != nil || ok {
		t.Fatalf("exists missing: %v %v", ok, err)
	}

	if _, found, err := c.Get(ctx, "cache"); err != nil || found {
		t.Fatalf("get miss: %v %v", found, err)
	}
	if err := c.SetEx(ctx, "cache", "v1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, found, err := c2.Get(ctx, "cache"); err != nil || !found || v != "v1" {
		t.Fatalf("get hit via second client: %q %v %v", v, found, err)
	}
	if err := c.Del(ctx, "cache"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c2.Get(ctx, "cache"); err != nil || found {
		t.Fatalf("get after del: %v %v", found, err)
	}
}
