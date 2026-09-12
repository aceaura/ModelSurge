// Package config 定义 relayd 的配置文件模型与加载校验。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 顶层配置。
type Config struct {
	Listen    string     `yaml:"listen"`  // 客户端入口监听地址，如 "127.0.0.1:8080"
	APIKey    string     `yaml:"api_key"` // 客户端鉴权 key；为空则不鉴权（仅限本机调试）
	Upstreams []Upstream `yaml:"upstreams"`

	// FirstTokenTimeout 等上游首个 SSE 事件的超时（如 "30s"）。
	// 超时且尚未向客户端写字节时换下一个上游重试；空 = 30s，"0" 禁用。
	FirstTokenTimeout string `yaml:"first_token_timeout"`
	// EstimateUsage 上游未给 usage 时本地粗估兜底（估算值仅作参考，默认关）。
	EstimateUsage bool `yaml:"estimate_usage"`
	// AccessLog 访问日志开关（method/path/status/耗时）；nil = 默认开。
	AccessLog *bool `yaml:"access_log"`
	// TruncationRecovery 截断恢复开关（上游截断工具参数/正文后，
	// 下次请求注入合成提示告知模型）；nil = 默认开。
	TruncationRecovery *bool `yaml:"truncation_recovery"`
	// Scheduler 账号池动态调度（SQLite 持久化 + 限流冷却）；不配置则纯静态转发。
	Scheduler *Scheduler `yaml:"scheduler"`
	// Admin 管理面（/admin 前缀，X-Admin-Key 鉴权）；scheduler 开启时必填。
	Admin *Admin `yaml:"admin"`
	// Kiro kiro 协议账号的全局段（region/超时/退避/概率/TTL/fake_reasoning/
	// web_search/cloud/debug）；不配置则全默认。
	Kiro *Kiro `yaml:"kiro"`

	// FirstTokenTimeoutDur FirstTokenTimeout 的解析结果（Load 填充；测试可直接设置）。
	FirstTokenTimeoutDur time.Duration `yaml:"-"`
	// AccessLogEnabled AccessLog 的最终值（Load 填充；测试可直接设置）。
	AccessLogEnabled bool `yaml:"-"`
	// TruncationRecoveryEnabled TruncationRecovery 的最终值（Load 填充）。
	TruncationRecoveryEnabled bool `yaml:"-"`
}

// Scheduler 账号池动态调度配置。
// 粘性策略：恒选序号最小的可用账号，冷却结束自动切回（保 prompt cache 命中）。
type Scheduler struct {
	DBPath string `yaml:"db_path"` // SQLite 文件路径
	// SameAccountRetries 非限流错误（5xx/网络/超时）的原地重试次数；默认 1。
	SameAccountRetries int `yaml:"same_account_retries"`
	// Cooldowns 各级限流窗口触发后的冷却时长。
	Cooldowns Cooldowns `yaml:"cooldowns"`

	// CooldownsDur Cooldowns 的解析结果（Load 填充）。
	CooldownsDur CooldownsDur `yaml:"-"`
}

// Cooldowns 限流冷却时长（字符串形如 "5h"，Load 解析为 CooldownsDur）。
type Cooldowns struct {
	Default  string `yaml:"default"`   // 窗口识别失败时；默认 "5h"
	Window7h string `yaml:"window_7h"` // 7 小时窗口；默认 "7h"
	Monthly  string `yaml:"monthly"`   // 月度窗口；默认 "24h"
}

// CooldownsDur Cooldowns 的解析结果（Load 填充）。
type CooldownsDur struct {
	Default  time.Duration
	Window7h time.Duration
	Monthly  time.Duration
}

// Upstream 一个上游端点。
type Upstream struct {
	Name     string            `yaml:"name"`     // 标识
	Protocol string            `yaml:"protocol"` // anthropic / openai-chat / openai-responses / gemini / kiro
	BaseURL  string            `yaml:"base_url"` // 如 "https://api.anthropic.com"（kiro 不需要）
	APIKey   string            `yaml:"api_key"`
	Models   map[string]string `yaml:"models"` // canonical model -> 上游 native model；为空则接受任意模型并透传模型名
	// Kiro protocol=kiro 时的账号种子（凭据三选一；仅 scheduler 模式生效）。
	Kiro *UpstreamKiro `yaml:"kiro"`
}

// UpstreamKiro kiro 账号种子凭据（refresh_token / creds_file / cli_db 三选一，
// 多配按此优先级取第一个）。
type UpstreamKiro struct {
	RefreshToken  string `yaml:"refresh_token"`  // 直配 refresh token
	CredsFile     string `yaml:"creds_file"`     // Kiro IDE JSON 凭据文件路径
	CliDB         string `yaml:"cli_db"`         // kiro cli SQLite 路径
	Region        string `yaml:"region"`         // SSO 刷新区；空 = kiro.region 或 us-east-1
	APIRegion     string `yaml:"api_region"`     // API 区覆盖；空则按检测链推导
	ProfileArn    string `yaml:"profile_arn"`    // 可空，首次使用时自动获取回填
	WebSearch     bool   `yaml:"web_search"`     // web_search 工具注入（MCP 代执行）
	FakeReasoning bool   `yaml:"fake_reasoning"` // fake_reasoning 思考标签注入
}

// Admin 管理面配置。
type Admin struct {
	APIKey string `yaml:"api_key"` // X-Admin-Key 值；scheduler 开启时必填
}

// Kiro kiro 协议账号的全局配置。region 为账号级可覆盖的默认，
// fake_reasoning / web_search_inject 为账号级可另开的下限，
// 其余为进程级运行参数（超时/退避/概率/缓存 TTL/云中转/调试）。
type Kiro struct {
	Region string `yaml:"region"` // SSO 刷新区默认；空 = us-east-1
	// FirstTokenTimeout kiro 候选等上游首个事件的超时；空 = 沿用全局 first_token_timeout。
	FirstTokenTimeout string `yaml:"first_token_timeout"`
	// StreamingReadTimeout 流式响应 chunk 间读超时（看门狗，超时断流）；空/0 = 禁用。
	StreamingReadTimeout string `yaml:"streaming_read_timeout"`
	// RecoveryTimeout 熔断指数退避基数；空 = 60s。
	RecoveryTimeout string `yaml:"recovery_timeout"`
	// MaxBackoffMultiplier 熔断冷却封顶倍数（冷却 = 基数×倍数）；0 = 1440（60s 基数即 24h）。
	MaxBackoffMultiplier int `yaml:"max_backoff_multiplier"`
	// ProbabilisticRetry 熔断半开试探概率（0-1）；0 = 0.1。
	ProbabilisticRetry float64 `yaml:"probabilistic_retry"`
	// CacheTTL kiro 模型缓存 TTL；空 = 12h。
	CacheTTL string `yaml:"cache_ttl"`
	// FakeReasoning fake_reasoning 思考标签注入（全局默认，账号级可另开）。
	FakeReasoning bool `yaml:"fake_reasoning"`
	// FakeReasoningMaxTokens 合成思考默认预算（tokens）；0 = 4000。
	FakeReasoningMaxTokens int `yaml:"fake_reasoning_max_tokens"`
	// WebSearchInject web_search 工具注入（全局默认，账号级可另开）。
	WebSearchInject bool       `yaml:"web_search_inject"`
	Cloud           *KiroCloud `yaml:"cloud"`
	// Debug Kiro 出站载荷调试日志（脱敏后落盘）。
	Debug bool `yaml:"debug"`
	// DebugDir 调试日志目录；空 = kiro-debug。
	DebugDir string `yaml:"debug_dir"`

	// 解析结果（Load 填充）。
	FirstTokenTimeoutDur    time.Duration `yaml:"-"`
	StreamingReadTimeoutDur time.Duration `yaml:"-"`
	RecoveryTimeoutDur      time.Duration `yaml:"-"`
	CacheTTLDur             time.Duration `yaml:"-"`
}

// KiroCloud 云中转（KiroaaS Cloud forward Lambda）：Kiro 出站流量经
// 转发服务中转，网络失败回退直连。
type KiroCloud struct {
	Enabled    bool   `yaml:"enabled"`
	ForwardURL string `yaml:"forward_url"` // 转发端点，如 https://cloud.kiroaas.hnew.city/forward
	APIKey     string `yaml:"api_key"`     // 会话密钥（X-Cloud-Key）
}

// Load 读取并校验配置。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	c.AccessLogEnabled = c.AccessLog == nil || *c.AccessLog
	c.TruncationRecoveryEnabled = c.TruncationRecovery == nil || *c.TruncationRecovery
	c.FirstTokenTimeoutDur = 30 * time.Second
	if c.FirstTokenTimeout != "" {
		d, err := time.ParseDuration(c.FirstTokenTimeout)
		if err != nil {
			return nil, fmt.Errorf("config: first_token_timeout: %w", err)
		}
		c.FirstTokenTimeoutDur = d
	}
	if sc := c.Scheduler; sc != nil {
		if sc.DBPath == "" {
			return nil, fmt.Errorf("config: scheduler.db_path is required")
		}
		if c.Admin == nil || c.Admin.APIKey == "" {
			return nil, fmt.Errorf("config: admin.api_key is required when scheduler is enabled")
		}
		if sc.SameAccountRetries < 0 {
			return nil, fmt.Errorf("config: scheduler.same_account_retries must be >= 0")
		}
		if sc.SameAccountRetries == 0 {
			sc.SameAccountRetries = 1
		}
		cd := &sc.CooldownsDur
		for _, d := range []struct {
			raw string
			dst *time.Duration
			def time.Duration
			key string
		}{
			{sc.Cooldowns.Default, &cd.Default, 5 * time.Hour, "default"},
			{sc.Cooldowns.Window7h, &cd.Window7h, 7 * time.Hour, "window_7h"},
			{sc.Cooldowns.Monthly, &cd.Monthly, 24 * time.Hour, "monthly"},
		} {
			if d.raw == "" {
				*d.dst = d.def
				continue
			}
			v, err := time.ParseDuration(d.raw)
			if err != nil || v <= 0 {
				return nil, fmt.Errorf("config: scheduler.cooldowns.%s: invalid duration %q", d.key, d.raw)
			}
			*d.dst = v
		}
	}
	if c.Kiro != nil {
		if err := c.Kiro.parse(); err != nil {
			return nil, err
		}
	}
	if len(c.Upstreams) == 0 {
		return nil, fmt.Errorf("config: at least one upstream is required")
	}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Name == "" {
			u.Name = fmt.Sprintf("upstream-%d", i)
		}
		if u.Protocol == "" {
			return nil, fmt.Errorf("config: upstream %q: protocol is required", u.Name)
		}
		if u.Protocol == "kiro" {
			if u.Kiro == nil || (u.Kiro.RefreshToken == "" && u.Kiro.CredsFile == "" && u.Kiro.CliDB == "") {
				return nil, fmt.Errorf("config: upstream %q: kiro seed requires one of kiro.refresh_token / kiro.creds_file / kiro.cli_db", u.Name)
			}
			if u.BaseURL != "" || u.APIKey != "" || len(u.Models) > 0 {
				return nil, fmt.Errorf("config: upstream %q: kiro upstream accepts no base_url/api_key/models", u.Name)
			}
			continue
		}
		if u.Kiro != nil {
			return nil, fmt.Errorf("config: upstream %q: kiro seed is only valid with protocol: kiro", u.Name)
		}
		if u.BaseURL == "" {
			return nil, fmt.Errorf("config: upstream %q: base_url is required", u.Name)
		}
		u.BaseURL = strings.TrimRight(u.BaseURL, "/")
	}
	return &c, nil
}

// parse 解析 kiro 段的 duration/数值字段并填默认值。
func (k *Kiro) parse() error {
	for _, d := range []struct {
		raw  string
		dst  *time.Duration
		def  time.Duration
		zero bool // "0" 是否合法（看门狗以 0 表示禁用）
		key  string
	}{
		{k.FirstTokenTimeout, &k.FirstTokenTimeoutDur, 0, true, "first_token_timeout"},
		{k.StreamingReadTimeout, &k.StreamingReadTimeoutDur, 0, true, "streaming_read_timeout"},
		{k.RecoveryTimeout, &k.RecoveryTimeoutDur, 60 * time.Second, false, "recovery_timeout"},
		{k.CacheTTL, &k.CacheTTLDur, 12 * time.Hour, false, "cache_ttl"},
	} {
		if d.raw == "" {
			*d.dst = d.def
			continue
		}
		v, err := time.ParseDuration(d.raw)
		if err != nil || v < 0 || (v == 0 && !d.zero) {
			return fmt.Errorf("config: kiro.%s: invalid duration %q", d.key, d.raw)
		}
		*d.dst = v
	}
	switch {
	case k.MaxBackoffMultiplier < 0 || k.MaxBackoffMultiplier > 1_000_000:
		return fmt.Errorf("config: kiro.max_backoff_multiplier: must be in [1, 1000000] (0 = default 1440)")
	case k.MaxBackoffMultiplier == 0:
		k.MaxBackoffMultiplier = 1440
	}
	if k.ProbabilisticRetry < 0 || k.ProbabilisticRetry > 1 {
		return fmt.Errorf("config: kiro.probabilistic_retry: must be in [0, 1] (0 = default 0.1)")
	}
	if k.FakeReasoningMaxTokens < 0 {
		return fmt.Errorf("config: kiro.fake_reasoning_max_tokens: must be >= 0 (0 = default 4000)")
	}
	return nil
}
