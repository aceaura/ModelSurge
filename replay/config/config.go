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

	UpstreamTimeoutDuration time.Duration `yaml:"-"`
	CacheTTLDuration        time.Duration `yaml:"-"`
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
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8081"
	}
	if cfg.DBPath == "" || cfg.UpstreamURL == "" || cfg.AgentServiceKey == "" || cfg.UpstreamServiceKey == "" || cfg.AdminKey == "" {
		return nil, fmt.Errorf("config: db_path, upstream_url, agent_service_key, upstream_service_key and admin_key are required")
	}
	cfg.UpstreamTimeoutDuration = 10 * time.Second
	if cfg.UpstreamTimeout != "" {
		cfg.UpstreamTimeoutDuration, err = time.ParseDuration(cfg.UpstreamTimeout)
		if err != nil || cfg.UpstreamTimeoutDuration <= 0 {
			return nil, fmt.Errorf("config: upstream_timeout: invalid duration %q", cfg.UpstreamTimeout)
		}
	}
	if cfg.CacheTTL != "" {
		cfg.CacheTTLDuration, err = time.ParseDuration(cfg.CacheTTL)
		if err != nil || cfg.CacheTTLDuration < 0 {
			return nil, fmt.Errorf("config: cache_ttl: invalid duration %q", cfg.CacheTTL)
		}
	}
	return &cfg, nil
}
