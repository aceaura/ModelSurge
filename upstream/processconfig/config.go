package processconfig

import (
	"fmt"
	"os"
	"time"

	"github.com/aceaura/ModelSurge/upstream/config"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/redisx"
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
	Listen           string           `yaml:"listen"`
	DBPath           string           `yaml:"db_path"`
	DBDriver         string           `yaml:"db_driver"`
	DBDSN            string           `yaml:"db_dsn"`
	ServiceKey       string           `yaml:"service_key"`
	AdminKey         string           `yaml:"admin_key"`
	LegacyDB         string           `yaml:"legacy_db_import"`
	AccessLog        *bool            `yaml:"access_log"`
	AccessLogEnabled bool             `yaml:"-"`
	Kiro             *config.Kiro     `yaml:"kiro"`
	Cooldowns        config.Cooldowns `yaml:"cooldowns"`
	Redis            redisx.Config    `yaml:"redis"`
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
	if err := c.Normalize(); err != nil {
		return c, fmt.Errorf("config: %s: %w", path, err)
	}
	// 三进程二进制的硬约束：sqlite 仅属单进程模式（cmd/modelsurge），此处
	// 只接受 postgres——env 漏配时 fail fast，杜绝静默服务陈旧本地库。
	if c.DBDriver != dialect.Postgres {
		return c, fmt.Errorf("config: %s: db_driver: cluster process requires postgres, got %q (sqlite is single-process mode only)", path, c.DBDriver)
	}
	return c, nil
}

// Normalize 补默认值并校验（指针接收者：解析出的 db_dsn 要回写调用方）；
// 单进程组合根在覆写回环地址后复用（它必须显式声明 db_driver=sqlite）。
// 数据源解析：db_driver 必填，绝无缺省——集群进程漏配 DSN 时宁可启动失败，
// 也不能静默回落陈旧 SQLite；sqlite 且 db_dsn 空时取 db_path。
func (c *Upstream) Normalize() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:18100"
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
	if c.DBDSN == "" {
		if c.DBPath == "" {
			return fmt.Errorf("db_path or db_dsn is required")
		}
		c.DBDSN = c.DBPath
	}
	c.Redis.Normalize()
	if c.ServiceKey == "" {
		return fmt.Errorf("service_key is required")
	}
	if c.Kiro != nil {
		if err := config.ParseKiro(c.Kiro); err != nil {
			return err
		}
	}
	return nil
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
