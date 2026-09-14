package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "modelsurge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigForcesLoopbackWiringAndNormalizes(t *testing.T) {
	path := writeConfig(t, `
agent:
  listen: 0.0.0.0:18099
  db_path: agent.db
  replay_url: http://elsewhere:19999
  service_key: agent-key
replay:
  db_path: replay.db
  upstream_url: http://elsewhere:19998
  agent_service_key: agent-key
  upstream_service_key: up-key
  admin_key: admin-key
  upstream_timeout: 10s
  cache_ttl: 12h
upstream:
  listen: 0.0.0.1:9999
  db_path: upstream.db
  service_key: up-key
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.Listen != upstreamLoopback {
		t.Errorf("upstream listen not forced to loopback: %s", cfg.Upstream.Listen)
	}
	if cfg.Replay.Listen != replayLoopback {
		t.Errorf("replay listen not forced to loopback: %s", cfg.Replay.Listen)
	}
	if cfg.Replay.UpstreamURL != "http://"+upstreamLoopback {
		t.Errorf("replay upstream_url not forced to loopback: %s", cfg.Replay.UpstreamURL)
	}
	if cfg.Agent.ReplayURL != "http://"+replayLoopback {
		t.Errorf("agent replay_url not forced to loopback: %s", cfg.Agent.ReplayURL)
	}
	if !cfg.Agent.AccessLogEnabled || cfg.Agent.ControlTimeoutDur <= 0 {
		t.Errorf("agent defaults missing: %+v", cfg.Agent)
	}
}

func TestLoadConfigKeepsExplicitLoopbackSilent(t *testing.T) {
	path := writeConfig(t, `
agent:
  db_path: agent.db
  service_key: agent-key
replay:
  db_path: replay.db
  agent_service_key: agent-key
  upstream_service_key: up-key
  admin_key: admin-key
upstream:
  db_path: upstream.db
  service_key: up-key
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.Listen != "0.0.0.0:18099" {
		t.Errorf("agent listen default changed: %s", cfg.Agent.Listen)
	}
	if cfg.Replay.Listen != replayLoopback || cfg.Upstream.Listen != upstreamLoopback {
		t.Errorf("loopback defaults wrong: %s %s", cfg.Replay.Listen, cfg.Upstream.Listen)
	}
}

func TestLoadConfigRejectsMissingRequiredKeys(t *testing.T) {
	path := writeConfig(t, `
agent:
  db_path: agent.db
replay:
  db_path: replay.db
upstream:
  db_path: upstream.db
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required-field error, got %v", err)
	}
}
