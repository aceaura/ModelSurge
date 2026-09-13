package processconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRelayExpandsEnvironment(t *testing.T) {
	t.Setenv("TEST_SERVICE_KEY", "service-secret")
	t.Setenv("TEST_CLIENT_KEY", "client-secret")

	path := filepath.Join(t.TempDir(), "relay.yaml")
	body := []byte("db_path: relay.db\nupstream_url: http://upstream:18100\nservice_key: ${TEST_SERVICE_KEY}\napi_key: ${TEST_CLIENT_KEY}\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRelay(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceKey != "service-secret" || cfg.APIKey != "client-secret" {
		t.Fatalf("environment variables were not expanded: %#v", cfg)
	}
}
