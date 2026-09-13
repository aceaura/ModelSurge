package upstream

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"relayd/backend/account"
	"relayd/backend/server"
	"relayd/backend/upstreamstore"
)

func TestInternalAuthRejectsMissingKey(t *testing.T) {
	s := NewHTTPServer(nil, "secret")
	r := httptest.NewRequest(http.MethodGet, "/internal/v1/health", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", w.Code)
	}
	if body := w.Body.String(); body == "" || body == "secret" {
		t.Fatalf("unsafe body %q", body)
	}
}

func TestAccountAdminBelongsToUpstream(t *testing.T) {
	store, err := upstreamstore.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Accounts.InsertAccount(&account.Account{Name: "a", Type: account.TypeAPIKey, Enabled: true, Protocol: "openai-chat", BaseURL: "https://example.test", APIKey: "secret"}); err != nil {
		t.Fatal(err)
	}
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(NewService(store, mgr), "service-key")
	h.AdminHandler = server.NewAccountAdmin(mgr, "admin-key")
	r := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("missing admin key status=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	r.Header.Set("X-Admin-Key", "admin-key")
	w = httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() == "" || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
