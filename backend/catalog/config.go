package catalog

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"relayd/strategy"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type ByteSize int64

func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := parseByteSize(s)
	if err != nil {
		return err
	}
	*b = ByteSize(v)
	return nil
}

func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, suf.s) {
			mult = suf.m
			s = strings.TrimSpace(strings.TrimSuffix(s, suf.s))
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// CircuitSpec is the YAML-facing form; strategy.CircuitConfig is the runtime form.
type CircuitSpec struct {
	FailThreshold  int      `yaml:"fail_threshold"`
	OpenBase       Duration `yaml:"open_base"`
	OpenMax        Duration `yaml:"open_max"`
	HalfOpenProbes int      `yaml:"half_open_probes"`
	SuccessToClose int      `yaml:"success_to_close"`
}

func (s CircuitSpec) ToStrategy() strategy.CircuitConfig {
	return strategy.CircuitConfig{
		FailThreshold:  s.FailThreshold,
		OpenBase:       s.OpenBase.D(),
		OpenMax:        s.OpenMax.D(),
		HalfOpenProbes: s.HalfOpenProbes,
		SuccessToClose: s.SuccessToClose,
	}
}

type Config struct {
	Listen          string                  `yaml:"listen"`
	AdminListen     string                  `yaml:"admin_listen"`
	AuthTokens      []string                `yaml:"auth_tokens"`
	RequestTimeout  Duration                `yaml:"request_timeout"`
	MaxBufferedBody ByteSize                `yaml:"max_buffered_body"`
	Circuit         CircuitSpec             `yaml:"circuit"`
	ProbeSchedule   ScheduleConfig          `yaml:"probe_schedule"`
	Models          map[string]ModelSpec    `yaml:"models"`
	Providers       map[string]ProviderSpec `yaml:"providers"`
	Credentials     []CredentialSpec        `yaml:"credentials"`
	CredentialFiles []string                `yaml:"credential_files"`
}

type ScheduleConfig struct {
	Concurrency     int     `yaml:"concurrency"`
	Jitter          float64 `yaml:"jitter"`
	BudgetPerMinute int     `yaml:"budget_per_minute"`
	Mode            string  `yaml:"mode"` // all | passive_recovery
}

type ModelSpec struct {
	ProtocolHint string   `yaml:"protocol_hint"`
	Aliases      []string `yaml:"aliases"`
}

type ProviderSpec struct {
	Protocol string        `yaml:"protocol"`
	Probes   []ProbeConfig `yaml:"probes"`
	Circuit  *CircuitSpec  `yaml:"circuit"`
}

type CredentialSpec struct {
	Provider string        `yaml:"provider"`
	Name     string        `yaml:"name"`
	BaseURL  string        `yaml:"base_url"`
	APIKey   string        `yaml:"api_key"`
	Models   []string      `yaml:"models"`
	Priority int           `yaml:"priority"`
	Weight   int           `yaml:"weight"`
	AutoBan  *bool         `yaml:"auto_ban"`
	Enabled  *bool         `yaml:"enabled"`
	Circuit  *CircuitSpec  `yaml:"circuit"`
	Probes   []ProbeConfig `yaml:"probes"`
}

type ProbeConfig struct {
	Type          string        `yaml:"type"` // balance | ping
	Interval      Duration      `yaml:"interval"`
	Request       *HTTPProbeReq `yaml:"request"`
	Extract       *Extract      `yaml:"extract"`
	Minus         *BalanceTerm  `yaml:"minus"`
	Rule          *BalanceRule  `yaml:"rule"`
	Model         string        `yaml:"model"`
	SlowThreshold Duration      `yaml:"slow_threshold"`
}

type HTTPProbeReq struct {
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
	Auth    AuthSpec          `yaml:"auth"`
}

type AuthSpec struct {
	In       string `yaml:"in"` // header | query
	Name     string `yaml:"name"`
	Template string `yaml:"template"` // contains {api_key}
}

type Extract struct {
	Value string  `yaml:"value"`
	Scale float64 `yaml:"scale"`
}

type BalanceTerm struct {
	Request HTTPProbeReq `yaml:"request"`
	Extract Extract      `yaml:"extract"`
}

type BalanceRule struct {
	DisableBelow float64 `yaml:"disable_below"`
}

func (c CredentialSpec) autoBan() bool {
	return c.AutoBan == nil || *c.AutoBan
}

func (c CredentialSpec) enabled() bool {
	return c.Enabled == nil || *c.Enabled
}
