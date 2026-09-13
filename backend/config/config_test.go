package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
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

// minimalCfg scheduler 必选段的最小样例。
const minimalCfg = `
scheduler:
  db_path: /data/x.db
admin:
  api_key: sk-admin
`

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(writeCfg(t, minimalCfg))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kiro != nil {
		t.Fatal("no kiro section expected")
	}
	if c.Listen != "127.0.0.1:8080" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.FirstTokenTimeoutDur != 30*time.Second {
		t.Errorf("FirstTokenTimeoutDur = %v, want 30s", c.FirstTokenTimeoutDur)
	}
	if !c.AccessLogEnabled || !c.TruncationRecoveryEnabled {
		t.Errorf("default switches should be on")
	}
	if c.Scheduler.SameAccountRetries != 1 {
		t.Errorf("SameAccountRetries = %d, want default 1", c.Scheduler.SameAccountRetries)
	}
}

// scheduler 段与 admin.api_key 必填。
func TestLoad_SchedulerRequired(t *testing.T) {
	if _, err := Load(writeCfg(t, "listen: 127.0.0.1:8080\n")); err == nil {
		t.Error("missing scheduler must fail")
	}
	if _, err := Load(writeCfg(t, "scheduler:\n  db_path: /data/x.db\n")); err == nil {
		t.Error("missing admin.api_key must fail")
	}
	if _, err := Load(writeCfg(t, "scheduler: {}\nadmin:\n  api_key: k\n")); err == nil {
		t.Error("missing db_path must fail")
	}
}

func TestLoad_KiroSection(t *testing.T) {
	p := writeCfg(t, minimalCfg+`
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
}

func TestLoad_KiroDefaults(t *testing.T) {
	c, err := Load(writeCfg(t, minimalCfg+"kiro: {}\n"))
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
		{"bad recovery_timeout", minimalCfg + "kiro:\n  recovery_timeout: -5s\n"},
		{"zero cache_ttl", minimalCfg + "kiro:\n  cache_ttl: 0s\n"},
		{"probe rate out of range", minimalCfg + "kiro:\n  probabilistic_retry: 1.5\n"},
		{"backoff multiplier negative", minimalCfg + "kiro:\n  max_backoff_multiplier: -1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeCfg(t, tc.cfg)); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// TestLoad_ExampleYAML 仓库示例配置可解析（防示例腐化）。
func TestLoad_ExampleYAML(t *testing.T) {
	c, err := Load("testdata/legacy-relayd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Scheduler == nil || c.Scheduler.DBPath == "" {
		t.Fatal("example scheduler section expected")
	}
	if c.Admin == nil || c.Admin.APIKey == "" {
		t.Fatal("example admin section expected")
	}
	if c.Kiro == nil || c.Kiro.RecoveryTimeoutDur != 60*time.Second || c.Kiro.MaxBackoffMultiplier != 1440 {
		t.Fatalf("example kiro defaults: %+v", c.Kiro)
	}
}

// captureLog 捕获 log 输出，测试毕恢复。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// TestWarnTimeoutCross：首事件超时应小于流式看门狗；>= 时告警。
func TestWarnTimeoutCross(t *testing.T) {
	tests := []struct {
		name  string
		first time.Duration
		read  time.Duration
		warn  bool
	}{
		{"ok-smaller", 30 * time.Second, 5 * time.Minute, false},
		{"warn-greater", 10 * time.Minute, time.Minute, true},
		{"warn-equal", time.Minute, time.Minute, true},
		{"skip-read-disabled", 10 * time.Minute, 0, false},
		{"skip-first-disabled", 0, time.Minute, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureLog(t)
			warnTimeoutCross(tt.first, tt.read)
			got := buf.String()
			if tt.warn && !strings.Contains(got, "first_token_timeout") {
				t.Errorf("expected warning, got %q", got)
			}
			if !tt.warn && got != "" {
				t.Errorf("unexpected warning: %q", got)
			}
		})
	}
}

// TestLoad_TimeoutCrossWarn：Load 端到端接线——首事件超时 >= 看门狗时打告警，
// 配置仍加载成功；kiro 覆盖值优先生效。
func TestLoad_TimeoutCrossWarn(t *testing.T) {
	buf := captureLog(t)
	_, err := Load(writeCfg(t, minimalCfg+`
first_token_timeout: 10m
kiro:
  streaming_read_timeout: 1m
`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "first_token_timeout (10m0s) >= kiro.streaming_read_timeout (1m0s)") {
		t.Errorf("expected cross warning, got %q", buf.String())
	}

	// kiro 覆盖值更小时不告警
	buf.Reset()
	_, err = Load(writeCfg(t, minimalCfg+`
first_token_timeout: 10m
kiro:
  first_token_timeout: 30s
  streaming_read_timeout: 1m
`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "warning") {
		t.Errorf("unexpected warning with smaller kiro override: %q", buf.String())
	}

	// kiro 显式 "0"（禁用）不告警
	buf.Reset()
	_, err = Load(writeCfg(t, minimalCfg+`
first_token_timeout: 10m
kiro:
  first_token_timeout: "0"
  streaming_read_timeout: 1m
`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "warning") {
		t.Errorf("unexpected warning with disabled kiro timeout: %q", buf.String())
	}
}
