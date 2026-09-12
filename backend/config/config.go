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
	Listen    string     `yaml:"listen"`   // 客户端入口监听地址，如 "127.0.0.1:8080"
	APIKey    string     `yaml:"api_key"`  // 客户端鉴权 key；为空则不鉴权（仅限本机调试）
	Upstreams []Upstream `yaml:"upstreams"`

	// FirstTokenTimeout 等上游首个 SSE 事件的超时（如 "30s"）。
	// 超时且尚未向客户端写字节时换下一个上游重试；空 = 30s，"0" 禁用。
	FirstTokenTimeout string `yaml:"first_token_timeout"`
	// EstimateUsage 上游未给 usage 时本地粗估兜底（估算值仅作参考，默认关）。
	EstimateUsage bool `yaml:"estimate_usage"`
	// AccessLog 访问日志开关（method/path/status/耗时）；nil = 默认开。
	AccessLog *bool `yaml:"access_log"`

	// FirstTokenTimeoutDur FirstTokenTimeout 的解析结果（Load 填充；测试可直接设置）。
	FirstTokenTimeoutDur time.Duration `yaml:"-"`
	// AccessLogEnabled AccessLog 的最终值（Load 填充；测试可直接设置）。
	AccessLogEnabled bool `yaml:"-"`
}

// Upstream 一个上游端点。
type Upstream struct {
	Name     string            `yaml:"name"`     // 标识
	Protocol string            `yaml:"protocol"` // anthropic / openai-chat / openai-responses / gemini
	BaseURL  string            `yaml:"base_url"` // 如 "https://api.anthropic.com"
	APIKey   string            `yaml:"api_key"`
	Models   map[string]string `yaml:"models"` // canonical model -> 上游 native model；为空则接受任意模型并透传模型名
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
	c.FirstTokenTimeoutDur = 30 * time.Second
	if c.FirstTokenTimeout != "" {
		d, err := time.ParseDuration(c.FirstTokenTimeout)
		if err != nil {
			return nil, fmt.Errorf("config: first_token_timeout: %w", err)
		}
		c.FirstTokenTimeoutDur = d
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
		if u.BaseURL == "" {
			return nil, fmt.Errorf("config: upstream %q: base_url is required", u.Name)
		}
		u.BaseURL = strings.TrimRight(u.BaseURL, "/")
	}
	return &c, nil
}
