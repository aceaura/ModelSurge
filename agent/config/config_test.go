package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const clusterPreamble = "db_dsn: postgres://agent@localhost/agent\nreplay_url: http://replay\nservice_key: key\n"

func TestLoadDurationDefaultsAndSameAccountRetries(t *testing.T) {
	cfg := loadTestConfig(t, clusterPreamble)
	if cfg.ControlTimeoutDur != 10*time.Second || cfg.FirstTokenTimeoutDur != 30*time.Second {
		t.Fatalf("durations = %s/%s", cfg.ControlTimeoutDur, cfg.FirstTokenTimeoutDur)
	}
	if cfg.SameAccountRetries != 1 {
		t.Fatalf("same_account_retries = %d, want 1", cfg.SameAccountRetries)
	}
}

func TestLoadDurationExplicitZero(t *testing.T) {
	cfg := loadTestConfig(t, clusterPreamble+"control_timeout: 0\nfirst_token_timeout: 0\nsame_account_retries: 2\n")
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
			_, err := Load(writeTestConfig(t, clusterPreamble+tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want field %q", err, tc.want)
			}
		})
	}
}

func TestLoadNegativeSameAccountRetriesFails(t *testing.T) {
	_, err := Load(writeTestConfig(t, clusterPreamble+"same_account_retries: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "same_account_retries") {
		t.Fatalf("err = %v", err)
	}
}

// db_dsn 必填，绝无缺省：漏配时宁可启动失败，也不能静默服务陈旧本地库。
func TestLoadRequiresDBDSN(t *testing.T) {
	_, err := Load(writeTestConfig(t, "replay_url: http://replay\nservice_key: key\n"))
	if err == nil || !strings.Contains(err.Error(), "db_dsn: required") {
		t.Fatalf("err = %v, want db_dsn required", err)
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
