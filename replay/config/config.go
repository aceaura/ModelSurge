// Package config loads replay process configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen             string `yaml:"listen"`
	DBPath             string `yaml:"db_path"`
	UpstreamURL        string `yaml:"upstream_url"`
	AgentServiceKey    string `yaml:"agent_service_key"`
	UpstreamServiceKey string `yaml:"upstream_service_key"`
	AdminKey           string `yaml:"admin_key"`
	UpstreamTimeout    string `yaml:"upstream_timeout"`
	CacheTTL           string `yaml:"cache_ttl"`
	BootstrapClientKey string `yaml:"bootstrap_client_key"`
	AccessLog          *bool  `yaml:"access_log"`

	UpstreamTimeoutDuration time.Duration `yaml:"-"`
	CacheTTLDuration        time.Duration `yaml:"-"`
	AccessLogEnabled        bool          `yaml:"-"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(data))), &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.Normalize(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// Normalize 补默认值并校验必填项；单进程组合根在覆写回环地址后复用。
func (c *Config) Normalize() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8081"
	}
	c.AccessLogEnabled = c.AccessLog == nil || *c.AccessLog
	if c.DBPath == "" || c.UpstreamURL == "" || c.AgentServiceKey == "" || c.UpstreamServiceKey == "" || c.AdminKey == "" {
		return fmt.Errorf("db_path, upstream_url, agent_service_key, upstream_service_key and admin_key are required")
	}
	c.UpstreamTimeoutDuration = 10 * time.Second
	if c.UpstreamTimeout != "" {
		var err error
		c.UpstreamTimeoutDuration, err = time.ParseDuration(c.UpstreamTimeout)
		if err != nil || c.UpstreamTimeoutDuration <= 0 {
			return fmt.Errorf("upstream_timeout: invalid duration %q", c.UpstreamTimeout)
		}
	}
	if c.CacheTTL != "" {
		var err error
		c.CacheTTLDuration, err = time.ParseDuration(c.CacheTTL)
		if err != nil || c.CacheTTLDuration < 0 {
			return fmt.Errorf("cache_ttl: invalid duration %q", c.CacheTTL)
		}
	}
	return nil
}
