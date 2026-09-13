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
	DBPath             string `yaml:"db_path"`
	ControlTimeout     string `yaml:"control_timeout"`
	FirstTokenTimeout  string `yaml:"first_token_timeout"`
	SameAccountRetries int    `yaml:"same_account_retries"`
	EstimateUsage      bool   `yaml:"estimate_usage"`
	AccessLog          *bool  `yaml:"access_log"`
	TruncationRecovery *bool  `yaml:"truncation_recovery"`
	Kiro               *Kiro  `yaml:"kiro"`

	ControlTimeoutDur         time.Duration `yaml:"-"`
	FirstTokenTimeoutDur      time.Duration `yaml:"-"`
	AccessLogEnabled          bool          `yaml:"-"`
	TruncationRecoveryEnabled bool          `yaml:"-"`
}

type Kiro struct {
	FirstTokenTimeout      string `yaml:"first_token_timeout"`
	StreamingReadTimeout   string `yaml:"streaming_read_timeout"`
	WebSearchInject        bool   `yaml:"web_search_inject"`
	FakeReasoning          bool   `yaml:"fake_reasoning"`
	FakeReasoningMaxTokens int    `yaml:"fake_reasoning_max_tokens"`
	FakeReasoningBudgetCap int    `yaml:"fake_reasoning_budget_cap"`

	FirstTokenTimeoutDur    time.Duration `yaml:"-"`
	StreamingReadTimeoutDur time.Duration `yaml:"-"`
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
	if c.Listen == "" {
		c.Listen = "0.0.0.0:18099"
	}
	if c.DBPath == "" {
		c.DBPath = "/data/agent.db"
	}
	if c.ReplayURL == "" || c.ServiceKey == "" {
		return nil, fmt.Errorf("config: replay_url and service_key are required")
	}
	c.ControlTimeoutDur, err = parseDuration(c.ControlTimeout, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("config: control_timeout: %w", err)
	}
	c.FirstTokenTimeoutDur, err = parseDuration(c.FirstTokenTimeout, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("config: first_token_timeout: %w", err)
	}
	if c.SameAccountRetries < 0 {
		return nil, fmt.Errorf("config: same_account_retries must be >= 0")
	}
	if c.SameAccountRetries == 0 {
		c.SameAccountRetries = 1
	}
	c.AccessLogEnabled = c.AccessLog == nil || *c.AccessLog
	c.TruncationRecoveryEnabled = c.TruncationRecovery == nil || *c.TruncationRecovery
	if c.Kiro != nil {
		var err error
		if c.Kiro.FirstTokenTimeout != "" {
			c.Kiro.FirstTokenTimeoutDur, err = time.ParseDuration(c.Kiro.FirstTokenTimeout)
			if err != nil {
				return nil, fmt.Errorf("config: kiro.first_token_timeout: %w", err)
			}
		}
		if c.Kiro.StreamingReadTimeout != "" {
			c.Kiro.StreamingReadTimeoutDur, err = time.ParseDuration(c.Kiro.StreamingReadTimeout)
			if err != nil {
				return nil, fmt.Errorf("config: kiro.streaming_read_timeout: %w", err)
			}
		}
		if c.Kiro.FakeReasoningMaxTokens < 0 || c.Kiro.FakeReasoningBudgetCap < 0 {
			return nil, fmt.Errorf("config: kiro fake reasoning token limits must be >= 0")
		}
		if c.Kiro.FakeReasoningMaxTokens == 0 {
			c.Kiro.FakeReasoningMaxTokens = 4000
		}
		if c.Kiro.FakeReasoningBudgetCap == 0 {
			c.Kiro.FakeReasoningBudgetCap = 10000
		}
	}
	return &c, nil
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
