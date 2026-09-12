package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCfg 临时配置文件。
func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "relayd.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Defaults(t *testing.T) {
	p := writeCfg(t, `
upstreams:
  - name: u
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key: k
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kiro != nil {
		t.Fatal("no kiro section expected")
	}
	if c.FirstTokenTimeoutDur != 30*time.Second {
		t.Errorf("FirstTokenTimeoutDur = %v, want 30s", c.FirstTokenTimeoutDur)
	}
}

func TestLoad_KiroSection(t *testing.T) {
	p := writeCfg(t, `
listen: 127.0.0.1:8080
scheduler:
  db_path: /data/x.db
admin:
  api_key: sk-admin
kiro:
  region: eu-central-1
  first_token_timeout: 15s
  streaming_read_timeout: 300s
  recovery_timeout: 120s
  max_backoff_multiplier: 720
  probabilistic_retry: 0.05
  cache_ttl: 6h
  fake_reasoning: true
  fake_reasoning_max_tokens: 2000
  web_search_inject: true
  cloud:
    enabled: true
    forward_url: https://cloud.example.com/forward
    api_key: ck-1
  debug: true
  debug_dir: /tmp/kirodbg
upstreams:
  - name: kiro-main
    protocol: kiro
    kiro:
      refresh_token: rt-placeholder
      web_search: true
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	k := c.Kiro
	if k == nil {
		t.Fatal("kiro section expected")
	}
	if k.Region != "eu-central-1" {
		t.Errorf("Region = %q", k.Region)
	}
	if k.FirstTokenTimeoutDur != 15*time.Second {
		t.Errorf("FirstTokenTimeoutDur = %v", k.FirstTokenTimeoutDur)
	}
	if k.StreamingReadTimeoutDur != 300*time.Second {
		t.Errorf("StreamingReadTimeoutDur = %v", k.StreamingReadTimeoutDur)
	}
	if k.RecoveryTimeoutDur != 120*time.Second {
		t.Errorf("RecoveryTimeoutDur = %v", k.RecoveryTimeoutDur)
	}
	if k.MaxBackoffMultiplier != 720 {
		t.Errorf("MaxBackoffMultiplier = %d", k.MaxBackoffMultiplier)
	}
	if k.ProbabilisticRetry != 0.05 {
		t.Errorf("ProbabilisticRetry = %v", k.ProbabilisticRetry)
	}
	if k.CacheTTLDur != 6*time.Hour {
		t.Errorf("CacheTTLDur = %v", k.CacheTTLDur)
	}
	if !k.FakeReasoning || k.FakeReasoningMaxTokens != 2000 || !k.WebSearchInject {
		t.Errorf("flags: %+v", k)
	}
	if k.Cloud == nil || !k.Cloud.Enabled || k.Cloud.ForwardURL != "https://cloud.example.com/forward" || k.Cloud.APIKey != "ck-1" {
		t.Errorf("cloud: %+v", k.Cloud)
	}
	if !k.Debug || k.DebugDir != "/tmp/kirodbg" {
		t.Errorf("debug: %v %q", k.Debug, k.DebugDir)
	}
	// kiro 种子解析
	u := c.Upstreams[0]
	if u.Kiro == nil || u.Kiro.RefreshToken != "rt-placeholder" || !u.Kiro.WebSearch {
		t.Errorf("upstream kiro seed: %+v", u.Kiro)
	}
}

func TestLoad_KiroDefaults(t *testing.T) {
	p := writeCfg(t, `
scheduler:
  db_path: /data/x.db
admin:
  api_key: sk-admin
kiro: {}
upstreams:
  - name: k
    protocol: kiro
    kiro:
      cli_db: /path/to/data.sqlite3
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	k := c.Kiro
	if k.RecoveryTimeoutDur != 60*time.Second {
		t.Errorf("RecoveryTimeoutDur default = %v", k.RecoveryTimeoutDur)
	}
	if k.MaxBackoffMultiplier != 1440 {
		t.Errorf("MaxBackoffMultiplier default = %d", k.MaxBackoffMultiplier)
	}
	if k.CacheTTLDur != 12*time.Hour {
		t.Errorf("CacheTTLDur default = %v", k.CacheTTLDur)
	}
	// 空 = 沿用全局 / 禁用
	if k.FirstTokenTimeoutDur != 0 || k.StreamingReadTimeoutDur != 0 {
		t.Errorf("empty durations should stay 0: %v %v", k.FirstTokenTimeoutDur, k.StreamingReadTimeoutDur)
	}
}

func TestLoad_KiroInvalid(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{"bad recovery_timeout", `
kiro:
  recovery_timeout: -5s
upstreams:
  - name: u
    protocol: anthropic
    base_url: http://x
`},
		{"zero cache_ttl", `
kiro:
  cache_ttl: 0s
upstreams:
  - name: u
    protocol: anthropic
    base_url: http://x
`},
		{"probe rate out of range", `
kiro:
  probabilistic_retry: 1.5
upstreams:
  - name: u
    protocol: anthropic
    base_url: http://x
`},
		{"backoff multiplier negative", `
kiro:
  max_backoff_multiplier: -1
upstreams:
  - name: u
    protocol: anthropic
    base_url: http://x
`},
		{"kiro seed without credentials", `
kiro: {}
upstreams:
  - name: k
    protocol: kiro
    kiro: {}
`},
		{"kiro seed with base_url", `
kiro: {}
upstreams:
  - name: k
    protocol: kiro
    base_url: http://x
    kiro:
      refresh_token: rt
`},
		{"kiro seed on non-kiro upstream", `
kiro: {}
upstreams:
  - name: u
    protocol: anthropic
    base_url: http://x
    kiro:
      refresh_token: rt
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeCfg(t, tc.cfg)); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// TestLoad_ExampleYAML 仓库示例配置可解析且 kiro 段/种子齐全（防示例腐化）。
func TestLoad_ExampleYAML(t *testing.T) {
	c, err := Load("../../relayd.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Scheduler == nil || c.Scheduler.DBPath == "" {
		t.Fatal("example scheduler section expected")
	}
	if c.Admin == nil || c.Admin.APIKey == "" {
		t.Fatal("example admin section expected")
	}
	if c.Kiro == nil || c.Kiro.Region != "us-east-1" || c.Kiro.Cloud != nil {
		t.Fatalf("example kiro section: %+v", c.Kiro)
	}
	if c.Kiro.RecoveryTimeoutDur != 60*time.Second || c.Kiro.MaxBackoffMultiplier != 1440 {
		t.Fatalf("example kiro defaults: %+v", c.Kiro)
	}
	var kiroSeed *UpstreamKiro
	for _, u := range c.Upstreams {
		if u.Protocol == "kiro" {
			kiroSeed = u.Kiro
		}
	}
	if kiroSeed == nil || kiroSeed.RefreshToken == "" {
		t.Fatal("example kiro account seed expected")
	}
}

// request_overrides 解析（upstream 级 yaml）。
func TestLoad_RequestOverrides(t *testing.T) {
	p := writeCfg(t, `
upstreams:
  - name: u
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key: k
    request_overrides:
      temperature: 1
      top_p: 0.95
      thinking:
        enabled: true
        budget_tokens: 4096
        effort: max
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	ov := c.Upstreams[0].RequestOverrides
	if ov == nil {
		t.Fatal("request_overrides expected")
	}
	if ov.Temperature == nil || *ov.Temperature != 1 {
		t.Errorf("temperature = %v, want 1", ov.Temperature)
	}
	if ov.TopP == nil || *ov.TopP != 0.95 {
		t.Errorf("top_p = %v, want 0.95", ov.TopP)
	}
	if ov.Thinking == nil || !ov.Thinking.Enabled || ov.Thinking.BudgetTokens != 4096 || ov.Thinking.Effort != "max" {
		t.Errorf("thinking = %+v, want enabled/4096/max", ov.Thinking)
	}
}

// 不配 request_overrides 时为 nil（透传）。
func TestLoad_RequestOverridesAbsent(t *testing.T) {
	p := writeCfg(t, `
upstreams:
  - name: u
    protocol: anthropic
    base_url: https://api.anthropic.com
    api_key: k
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstreams[0].RequestOverrides != nil {
		t.Errorf("request_overrides = %+v, want nil", c.Upstreams[0].RequestOverrides)
	}
}
