// Package redisx 统一 Redis 热态接入（design/deployment-modes.md 2.4）：
// 纯易失层、DB 权威——所有使用方必须把 Redis 错误视为「读未命中/降级」，
// 回退到本地行为，绝不让 Redis 故障阻塞请求路径。
// 未配置（nil Client）= 现行为，模式一零依赖不受影响。
package redisx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config Redis 连接配置（三进程 yaml 的 redis 段；env 插值由上层完成）。
type Config struct {
	Addr     string `yaml:"addr"`     // host:port；空 = 未配置（禁用热态）
	Password string `yaml:"password"` // 可选
	Prefix   string `yaml:"prefix"`   // key 前缀；默认 "modelsurge:"
}

// Normalize 补默认前缀。
func (c *Config) Normalize() {
	if c != nil && c.Prefix == "" {
		c.Prefix = "modelsurge:"
	}
}

// 降级参数：连接失败/命令错误后熔断窗口内直接快速失败（不再打 Redis），
// 窗口过后自动半开重试。命令超时收紧到亚秒级，热路径不被死 Redis 拖慢。
const (
	downFor  = 10 * time.Second
	cmdWait  = 500 * time.Millisecond
	pingWait = 3 * time.Second
)

// Client 热态客户端；零值不可用，经 New 构造。
type Client struct {
	rdb     *redis.Client
	prefix  string
	downUntil atomic.Int64 // unix nano；> now 视为降级中
}

// New 构造并做一次 Ping（失败仅告警不报错：降级运行，等待半开恢复）。
func New(cfg Config) *Client {
	if cfg.Prefix == "" {
		cfg.Prefix = "modelsurge:"
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.Addr, Password: cfg.Password,
		DialTimeout: cmdWait, ReadTimeout: cmdWait, WriteTimeout: cmdWait,
	})
	c := &Client{rdb: rdb, prefix: cfg.Prefix}
	ctx, cancel := context.WithTimeout(context.Background(), pingWait)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("redisx: ping %s failed, degrading until reachable: %v", cfg.Addr, err)
		c.markDown()
	}
	return c
}

func (c *Client) key(k string) string { return c.prefix + k }

// markDown 进入降级窗口。
func (c *Client) markDown() {
	c.downUntil.Store(time.Now().Add(downFor).UnixNano())
}

// degraded 是否处于降级窗口。
func (c *Client) degraded() bool {
	return time.Now().UnixNano() < c.downUntil.Load()
}

// Available 当前是否可用（降级窗口外）。测试用；语义同各 op 的即时报错。
func (c *Client) Available() bool { return !c.degraded() }

// markErr 统一错误处理：连接类错误进入降级窗口；业务键错误（redis.Nil）
// 与父 ctx 取消（客户端断连，非 Redis 故障）不降级。
func (c *Client) markErr(err error) error {
	if err != nil && err != redis.Nil && !errors.Is(err, context.Canceled) {
		c.markDown()
	}
	return err
}

// opCtx 单 op 硬超时：请求 ctx 可能长达 10s+，而 Redis 故障形态可以很慢
// （容器移除后 DNS i/o timeout × 池内多次拨号重试）——热路径绝不被拖垮，
// 超时即降级走 DB 兜底。
func opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, cmdWait)
}

// Incr 自增并返回新值（round_robin 全局游标）。降级/错误返回 err。
func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	if c.degraded() {
		return 0, fmt.Errorf("redisx: degraded")
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	v, err := c.rdb.Incr(ctx, c.key(key)).Result()
	return v, c.markErr(err)
}

// TryLock SET NX PX 试探锁（Half-Open 全局单试探）：
// true = 抢到（本次试探归我），false = 已被他持有。
func (c *Client) TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if c.degraded() {
		return false, fmt.Errorf("redisx: degraded")
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	ok, err := c.rdb.SetNX(ctx, c.key(key), "1", ttl).Result()
	return ok, c.markErr(err)
}

// Exists 键是否存在（试探放行核验：Evaluate 抢锁后 ExecuteKiro 依此放行）。
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	if c.degraded() {
		return false, fmt.Errorf("redisx: degraded")
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	n, err := c.rdb.Exists(ctx, c.key(key)).Result()
	return n > 0, c.markErr(err)
}

// Get 读缓存值；未命中返回 ("", false, nil)。
func (c *Client) Get(ctx context.Context, key string) (string, bool, error) {
	if c.degraded() {
		return "", false, fmt.Errorf("redisx: degraded")
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	v, err := c.rdb.Get(ctx, c.key(key)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	return v, true, c.markErr(err)
}

// SetEx 写缓存值（TTL 秒级）。
func (c *Client) SetEx(ctx context.Context, key, val string, ttl time.Duration) error {
	if c.degraded() {
		return fmt.Errorf("redisx: degraded")
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	return c.markErr(c.rdb.SetEx(ctx, c.key(key), val, ttl).Err())
}

// Del 删键（主动失效）。
func (c *Client) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if c.degraded() {
		return fmt.Errorf("redisx: degraded")
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = c.key(k)
	}
	return c.markErr(c.rdb.Del(ctx, full...).Err())
}

// Close 关闭连接。
func (c *Client) Close() error { return c.rdb.Close() }
