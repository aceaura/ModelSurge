package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"relayd/strategy"
)

const baseYAML = `
listen: "127.0.0.1:8080"
admin_listen: "127.0.0.1:8081"
auth_tokens: ["tok-1"]
request_timeout: 120s
max_buffered_body: 4MB
circuit:
  fail_threshold: 2
  open_base: 10s
  open_max: 5m
probe_schedule:
  concurrency: 4
  jitter: 0.2
  budget_per_minute: 30
  mode: passive_recovery
models:
  claude-sonnet-4:
    protocol_hint: claude
    aliases: [claude-3-5-sonnet-20241022, anthropic/claude-sonnet-4]
  gpt-4o:
    protocol_hint: openai
providers:
  station:
    protocol: openai
    probes:
      - type: balance
        interval: 5m
        request:
          method: GET
          path: /api/user/self
          auth: {in: header, name: Authorization, template: "Bearer {api_key}"}
        extract: {value: "data.quota", scale: 0.000002}
        rule: {disable_below: 0.01}
credentials:
  - provider: station
    name: st-01
    base_url: https://a.example.com
    api_key: sk-aaa
    models: [claude-3-5-sonnet-20241022, gpt-4o]
    priority: 0
    weight: 10
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_HappyPath(t *testing.T) {
	cfg, err := Load(writeTemp(t, baseYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestTimeout.D() != 120*time.Second {
		t.Fatalf("duration parse: %v", cfg.RequestTimeout.D())
	}
	if cfg.MaxBufferedBody != 4<<20 {
		t.Fatalf("byte size parse: %d", cfg.MaxBufferedBody)
	}
	if cfg.ProbeSchedule.Mode != "passive_recovery" {
		t.Fatal("mode parse")
	}
}

func TestLoad_MultiFileCredentials(t *testing.T) {
	p := writeTemp(t, baseYAML+"\ncredential_files: [\"creds/*.yaml\"]\n")
	dir := filepath.Dir(p)
	os.MkdirAll(filepath.Join(dir, "creds"), 0o755)
	os.WriteFile(filepath.Join(dir, "creds", "extra.yaml"), []byte(`
credentials:
  - provider: station
    name: st-02
    base_url: https://b.example.com
    api_key: sk-bbb
    models: [gpt-4o]
`), 0o644)

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Credentials) != 2 {
		t.Fatalf("want 2 credentials, got %d", len(cfg.Credentials))
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name   string
		mutate string
		want   string
	}{
		{"no tokens", "auth_tokens: []", "auth_tokens"},
		{"alias conflict", `models:
  m1: {aliases: [shared]}
  m2: {aliases: [shared]}`, "claimed by both"},
		{"duplicate credential", `credentials:
  - {provider: station, name: st-01, base_url: "https://x.com", api_key: k, models: [gpt-4o]}
  - {provider: station, name: st-01, base_url: "https://y.com", api_key: k, models: [gpt-4o]}`, "duplicate credential"},
		{"unknown provider", `credentials:
  - {provider: nope, name: x1, base_url: "https://x.com", api_key: k, models: [gpt-4o]}`, "unknown provider"},
		{"bad url", `credentials:
  - {provider: station, name: x2, base_url: "not-a-url", api_key: k, models: [gpt-4o]}`, "invalid base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			switch tc.name {
			case "no tokens":
				body = baseYAML
				body = replaceLine(body, `auth_tokens: ["tok-1"]`, "auth_tokens: []")
			case "alias conflict":
				body = baseYAML
				body = replaceBlockModels(body, tc.mutate)
			case "duplicate credential", "unknown provider", "bad url":
				body = baseYAML
				body = replaceBlockCredentials(body, tc.mutate)
			}
			_, err := Load(writeTemp(t, body))
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func replaceLine(s, old, new string) string {
	out := ""
	for _, l := range splitLines(s) {
		if l == old {
			l = new
		}
		out += l + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	return append(out, cur)
}

func replaceBlockModels(s, modelsBlock string) string {
	start := indexOf(s, "models:\n")
	end := indexOf(s, "providers:\n")
	return s[:start] + modelsBlock + "\n" + s[end:]
}

func replaceBlockCredentials(s, credBlock string) string {
	start := indexOf(s, "credentials:\n")
	return s[:start] + credBlock + "\n"
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

func TestBuildUpstreams_TemplateInheritanceAndCanonicalization(t *testing.T) {
	cfg, err := Load(writeTemp(t, baseYAML))
	if err != nil {
		t.Fatal(err)
	}
	cat := NewCatalog(cfg.Models)
	ups, err := cfg.BuildUpstreams(cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 {
		t.Fatalf("want 1 upstream, got %d", len(ups))
	}
	u := ups[0]
	if u.Protocol != "openai" || u.Weight != 10 || u.Priority != 0 {
		t.Fatalf("template/fields wrong: %+v", u)
	}
	want := map[string]string{
		"claude-sonnet-4": "claude-3-5-sonnet-20241022",
		"gpt-4o":          "gpt-4o",
	}
	for canonical, native := range want {
		got, ok := cat.NativeFor(u, canonical)
		if !ok || got != native {
			t.Fatalf("native mapping %s: got %q ok=%v", canonical, got, ok)
		}
	}
	if len(u.Models) != 2 {
		t.Fatalf("canonical models: %v", u.Models)
	}
	if u.Circuit.Snapshot().Status != strategy.StatusEnabled {
		t.Fatal("default enabled")
	}
	// credential-level probe override wins over provider template
	probes := cfg.ProbesFor("st-01")
	if len(probes) != 1 || probes[0].Type != "balance" {
		t.Fatalf("probes: %+v", probes)
	}
}

func TestBuildUpstreams_DisabledCredentialIsManual(t *testing.T) {
	disabled := baseYAML + `
  - provider: station
    name: st-off
    base_url: https://off.example.com
    api_key: sk-off
    models: [gpt-4o]
    enabled: false
`
	cfg, err := Load(writeTemp(t, disabled))
	if err != nil {
		t.Fatal(err)
	}
	ups, err := cfg.BuildUpstreams(NewCatalog(cfg.Models), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ups[1].Circuit.Status() != strategy.StatusManuallyDisabled {
		t.Fatalf("enabled:false must be ManuallyDisabled, got %s", ups[1].Circuit.Status())
	}
}
