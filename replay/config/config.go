// Package config loads replay process configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/redisx"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen             string        `yaml:"listen"`
	DBPath             string        `yaml:"db_path"`
	DBDriver           string        `yaml:"db_driver"`
	DBDSN              string        `yaml:"db_dsn"`
	UpstreamURL        string        `yaml:"upstream_url"`
	AgentServiceKey    string        `yaml:"agent_service_key"`
	UpstreamServiceKey string        `yaml:"upstream_service_key"`
	AdminKey           string        `yaml:"admin_key"`
	UpstreamTimeout    string        `yaml:"upstream_timeout"`
	CacheTTL           string        `yaml:"cache_ttl"`
	BootstrapClientKey string        `yaml:"bootstrap_client_key"`
	AccessLog          *bool         `yaml:"access_log"`
	Redis              redisx.Config `yaml:"redis"`

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
	// 三进程二进制的硬约束：sqlite 仅属单进程模式（cmd/modelsurge），此处
	// 只接受 postgres——env 漏配时 fail fast，杜绝静默服务陈旧本地库。
	if cfg.DBDriver != dialect.Postgres {
		return nil, fmt.Errorf("config: %s: db_driver: cluster process requires postgres, got %q (sqlite is single-process mode only)", path, cfg.DBDriver)
	}
	return &cfg, nil
}

// Normalize 补默认值并校验必填项；单进程组合根在覆写回环地址后复用（它必须
// 显式声明 db_driver=sqlite）。数据源解析：db_driver 必填，绝无缺省——集群
// 进程漏配 DSN 时宁可启动失败，也不能静默回落陈旧 SQLite；sqlite 且 db_dsn
// 空时取 db_path。
func (c *Config) Normalize() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8081"
	}
	c.AccessLogEnabled = c.AccessLog == nil || *c.AccessLog
	if c.DBDriver == "" {
		return fmt.Errorf("db_driver: required (postgres for cluster processes; sqlite only in single-process mode)")
	}
	if !dialect.Valid(c.DBDriver) {
		return fmt.Errorf("db_driver: unsupported %q", c.DBDriver)
	}
	if c.DBDriver == dialect.Postgres && c.DBDSN == "" {
		return fmt.Errorf("db_dsn is required when db_driver is postgres")
	}
	if c.DBDSN == "" && c.DBPath == "" {
		return fmt.Errorf("db_path or db_dsn is required")
	}
	if c.DBDSN == "" {
		c.DBDSN = c.DBPath
	}
	c.Redis.Normalize()
	if c.UpstreamURL == "" || c.AgentServiceKey == "" || c.UpstreamServiceKey == "" || c.AdminKey == "" {
		return fmt.Errorf("upstream_url, agent_service_key, upstream_service_key and admin_key are required")
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
