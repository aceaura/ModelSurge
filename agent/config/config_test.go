package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDurationDefaultsAndSameAccountRetries(t *testing.T) {
	cfg := loadTestConfig(t, "replay_url: http://replay\nservice_key: key\n")
	if cfg.ControlTimeoutDur != 10*time.Second || cfg.FirstTokenTimeoutDur != 30*time.Second {
		t.Fatalf("durations = %s/%s", cfg.ControlTimeoutDur, cfg.FirstTokenTimeoutDur)
	}
	if cfg.SameAccountRetries != 1 {
		t.Fatalf("same_account_retries = %d, want 1", cfg.SameAccountRetries)
	}
}

func TestLoadDurationExplicitZero(t *testing.T) {
	cfg := loadTestConfig(t, "replay_url: http://replay\nservice_key: key\ncontrol_timeout: 0\nfirst_token_timeout: 0\nsame_account_retries: 2\n")
	if cfg.ControlTimeoutDur != 0 || cfg.FirstTokenTimeoutDur != 0 {
		t.Fatalf("durations = %s/%s, want zero", cfg.ControlTimeoutDur, cfg.FirstTokenTimeoutDur)
	}
	if cfg.SameAccountRetries != 2 {
		t.Fatalf("same_account_retries = %d, want 2", cfg.SameAccountRetries)
	}
}

func TestLoadInvalidDurationFailsFast(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "control malformed", body: "control_timeout: eventually\n", want: "control_timeout"},
		{name: "control negative", body: "control_timeout: -1s\n", want: "control_timeout"},
		{name: "first token malformed", body: "first_token_timeout: soon\n", want: "first_token_timeout"},
		{name: "first token negative", body: "first_token_timeout: -1s\n", want: "first_token_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTestConfig(t, "replay_url: http://replay\nservice_key: key\n"+tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want field %q", err, tc.want)
			}
		})
	}
}

func TestLoadNegativeSameAccountRetriesFails(t *testing.T) {
	_, err := Load(writeTestConfig(t, "replay_url: http://replay\nservice_key: key\nsame_account_retries: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "same_account_retries") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadKiroEncodingAndTimeoutFields(t *testing.T) {
	cfg := loadTestConfig(t, `replay_url: http://replay
service_key: key
kiro:
  first_token_timeout: 45s
  streaming_read_timeout: 2m
  web_search_inject: true
  fake_reasoning: true
`)
	if cfg.Kiro == nil || cfg.Kiro.FirstTokenTimeoutDur != 45*time.Second || cfg.Kiro.StreamingReadTimeoutDur != 2*time.Minute {
		t.Fatalf("kiro=%+v", cfg.Kiro)
	}
	if !cfg.Kiro.WebSearchInject || !cfg.Kiro.FakeReasoning || cfg.Kiro.FakeReasoningMaxTokens != 4000 || cfg.Kiro.FakeReasoningBudgetCap != 10000 {
		t.Fatalf("kiro encoding fields=%+v", cfg.Kiro)
	}
}

func loadTestConfig(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := Load(writeTestConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
