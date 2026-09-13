package processconfig

import (
	"fmt"
	"os"
	"time"

	"github.com/aceaura/ModelSurge/upstream/config"
	"gopkg.in/yaml.v3"
)

type Relay struct {
	Listen            string `yaml:"listen"`
	APIKey            string `yaml:"api_key"`
	AdminKey          string `yaml:"admin_key"`
	DBPath            string `yaml:"db_path"`
	UpstreamURL       string `yaml:"upstream_url"`
	ServiceKey        string `yaml:"service_key"`
	ControlTimeout    string `yaml:"control_timeout"`
	Bootstrap         bool   `yaml:"compatibility_bootstrap"`
	FirstTokenTimeout string `yaml:"first_token_timeout"`
	EstimateUsage     bool   `yaml:"estimate_usage"`
	AccessLog         *bool  `yaml:"access_log"`
}
type Upstream struct {
	Listen     string           `yaml:"listen"`
	DBPath     string           `yaml:"db_path"`
	ServiceKey string           `yaml:"service_key"`
	AdminKey   string           `yaml:"admin_key"`
	LegacyDB   string           `yaml:"legacy_db_import"`
	Kiro       *config.Kiro     `yaml:"kiro"`
	Cooldowns  config.Cooldowns `yaml:"cooldowns"`
}

func load(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal([]byte(os.ExpandEnv(string(b))), v)
}
func LoadRelay(path string) (Relay, error) {
	var c Relay
	if err := load(path, &c); err != nil {
		return c, err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:18099"
	}
	if c.DBPath == "" {
		return c, fmt.Errorf("db_path is required")
	}
	if c.UpstreamURL == "" || c.ServiceKey == "" {
		return c, fmt.Errorf("upstream_url and service_key are required")
	}
	return c, nil
}
func LoadUpstream(path string) (Upstream, error) {
	var c Upstream
	if err := load(path, &c); err != nil {
		return c, err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:18100"
	}
	if c.DBPath == "" || c.ServiceKey == "" {
		return c, fmt.Errorf("db_path and service_key are required")
	}
	if c.Kiro != nil {
		if err := config.ParseKiro(c.Kiro); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (u Upstream) CooldownDurations() (config.CooldownsDur, error) {
	var out config.CooldownsDur
	for _, item := range []struct {
		raw  string
		dst  *time.Duration
		def  time.Duration
		name string
	}{
		{u.Cooldowns.Default, &out.Default, 5 * time.Hour, "default"},
		{u.Cooldowns.Window7h, &out.Window7h, 7 * time.Hour, "window_7h"},
		{u.Cooldowns.Monthly, &out.Monthly, 24 * time.Hour, "monthly"},
	} {
		if item.raw == "" {
			*item.dst = item.def
			continue
		}
		d, err := time.ParseDuration(item.raw)
		if err != nil || d <= 0 {
			return out, fmt.Errorf("cooldowns.%s: invalid duration %q", item.name, item.raw)
		}
		*item.dst = d
	}
	return out, nil
}
func (r Relay) ControlTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(r.ControlTimeout)
	if d <= 0 {
		return 10 * time.Second
	}
	return d
}
func (r Relay) LegacyConfig() *config.Config {
	d, _ := time.ParseDuration(r.FirstTokenTimeout)
	if d <= 0 {
		d = 30 * time.Second
	}
	access := r.AccessLog == nil || *r.AccessLog
	var admin *config.Admin
	if r.AdminKey != "" {
		admin = &config.Admin{APIKey: r.AdminKey}
	}
	return &config.Config{Listen: r.Listen, APIKey: r.APIKey, Admin: admin, FirstTokenTimeoutDur: d, EstimateUsage: r.EstimateUsage, AccessLogEnabled: access, TruncationRecoveryEnabled: true}
}
