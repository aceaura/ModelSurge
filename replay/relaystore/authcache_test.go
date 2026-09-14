package relaystore

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/upstream/dialect"
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

// 门控：鉴权缓存命中免 DB、PutUserModel/DeleteUserModel 主动失效
// （设计 2.4/需求 2.6；TTL 60s 兜底）。
func TestAuthenticateRedisCacheGated(t *testing.T) {
	ctx := context.Background()
	s, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	prefix := "modelsurge-auth-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	s.Redis = redisx.New(redisx.Config{Addr: testRedisAddr(t), Prefix: prefix})
	if !s.Redis.Available() {
		t.Fatalf("redis %s not reachable", testRedisAddr(t))
	}
	defer s.Redis.Close()

	if err := s.PutUserModel(ctx, UserModel{Name: "m1", Protocol: "anthropic", APIKey: "k1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// 首次：DB 路径 + 回填缓存
	if configured, ok, err := s.Authenticate(ctx, "m1", "anthropic", "k1"); err != nil || !configured || !ok {
		t.Fatalf("first auth: configured=%v ok=%v err=%v", configured, ok, err)
	}
	// 绕开 store 直接删行：缓存仍命中（证明走 Redis 而非 DB）
	if _, err := s.DB.Exec(`DELETE FROM user_models WHERE name='m1'`); err != nil {
		t.Fatal(err)
	}
	if configured, ok, err := s.Authenticate(ctx, "m1", "anthropic", "k1"); err != nil || !configured || !ok {
		t.Fatalf("cached auth after db delete: configured=%v ok=%v err=%v", configured, ok, err)
	}
	// PutUserModel 主动失效：新 key 生效、旧 key 拒绝
	if err := s.PutUserModel(ctx, UserModel{Name: "m1", Protocol: "anthropic", APIKey: "k2", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if configured, ok, err := s.Authenticate(ctx, "m1", "anthropic", "k2"); err != nil || !configured || !ok {
		t.Fatalf("new key after invalidate: configured=%v ok=%v err=%v", configured, ok, err)
	}
	if configured, ok, err := s.Authenticate(ctx, "m1", "anthropic", "k1"); err != nil || !configured || ok {
		t.Fatalf("old key after invalidate: configured=%v ok=%v err=%v", configured, ok, err)
	}
	// 协议不匹配（缓存命中分支）
	if configured, ok, err := s.Authenticate(ctx, "m1", "openai", "k2"); err != nil || !configured || ok {
		t.Fatalf("protocol mismatch: configured=%v ok=%v err=%v", configured, ok, err)
	}
	// DeleteUserModel 主动失效：未配置
	if err := s.DeleteUserModel(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if configured, ok, err := s.Authenticate(ctx, "m1", "anthropic", "k2"); err != nil || configured || ok {
		t.Fatalf("after delete: configured=%v ok=%v err=%v", configured, ok, err)
	}
	// 负缓存：未配置模型缓存后，PutUserModel 失效路径立即生效
	if configured, ok, err := s.Authenticate(ctx, "m2", "anthropic", "x"); err != nil || configured || ok {
		t.Fatalf("unknown model: configured=%v ok=%v err=%v", configured, ok, err)
	}
	if err := s.PutUserModel(ctx, UserModel{Name: "m2", Protocol: "anthropic", APIKey: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if configured, ok, err := s.Authenticate(ctx, "m2", "anthropic", "x"); err != nil || !configured || !ok {
		t.Fatalf("m2 after put: configured=%v ok=%v err=%v", configured, ok, err)
	}
	// 禁用模型 = 未配置（负缓存）
	if err := s.PutUserModel(ctx, UserModel{Name: "m3", Protocol: "anthropic", APIKey: "k", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if configured, ok, err := s.Authenticate(ctx, "m3", "anthropic", "k"); err != nil || configured || ok {
		t.Fatalf("disabled model: configured=%v ok=%v err=%v", configured, ok, err)
	}
}
