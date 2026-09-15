package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen             string `yaml:"listen"`
	ReplayURL          string `yaml:"replay_url"`
	ServiceKey         string `yaml:"service_key"`
	DBDSN              string `yaml:"db_dsn"`
	ControlTimeout     string `yaml:"control_timeout"`
	FirstTokenTimeout  string `yaml:"first_token_timeout"`
	SameAccountRetries int    `yaml:"same_account_retries"`
	EstimateUsage      bool   `yaml:"estimate_usage"`
	AccessLog          *bool  `yaml:"access_log"`
	TruncationRecovery *bool  `yaml:"truncation_recovery"`

	ControlTimeoutDur         time.Duration `yaml:"-"`
	FirstTokenTimeoutDur      time.Duration `yaml:"-"`
	AccessLogEnabled          bool          `yaml:"-"`
	TruncationRecoveryEnabled bool          `yaml:"-"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(b))), &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := c.Normalize(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &c, nil
}

// Normalize 补默认值并校验必填项。数据源：db_dsn 必填，绝无缺省——漏配时
// 宁可启动失败，也不能静默服务陈旧本地库。
func (c *Config) Normalize() error {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:18099"
	}
	if c.DBDSN == "" {
		return fmt.Errorf("db_dsn: required")
	}
	if c.ReplayURL == "" || c.ServiceKey == "" {
		return fmt.Errorf("replay_url and service_key are required")
	}
	var err error
	c.ControlTimeoutDur, err = parseDuration(c.ControlTimeout, 10*time.Second)
	if err != nil {
		return fmt.Errorf("control_timeout: %w", err)
	}
	c.FirstTokenTimeoutDur, err = parseDuration(c.FirstTokenTimeout, 30*time.Second)
	if err != nil {
		return fmt.Errorf("first_token_timeout: %w", err)
	}
	if c.SameAccountRetries < 0 {
		return fmt.Errorf("same_account_retries must be >= 0")
	}
	if c.SameAccountRetries == 0 {
		c.SameAccountRetries = 1
	}
	c.AccessLogEnabled = c.AccessLog == nil || *c.AccessLog
	c.TruncationRecoveryEnabled = c.TruncationRecovery == nil || *c.TruncationRecovery
	return nil
}

func parseDuration(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must be >= 0")
	}
	return d, nil
}
