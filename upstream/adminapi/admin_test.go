package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

// 7.4 管理面带 model_limits 落库：物化的 upstream_models 行带正确窗口值，
// GET 回显；PUT 缺省字段沿用已配窗口。
func testAdmin(t *testing.T) (*Admin, *upstreamstore.Store) {
	t.Helper()
	store, err := upstreamstore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr, err := account.NewManager(store.Accounts, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	admin := &Admin{sched: mgr, store: store.Accounts, adminKey: "admin-key", probeCl: http.DefaultClient}
	admin.accountChanged = func() error { return store.MaterializeAccounts() }
	admin.mountAdmin()
	return admin, store
}

func TestModelLimitsMaterializeAndEcho(t *testing.T) {
	admin, store := testAdmin(t)

	body := `{"name":"a","type":"api-key","protocol":"openai-chat","base_url":"https://example.test","api_key":"secret",
		"models":{"m1":"n1","m2":"n2"},"model_limits":{"m1":128000}}`
	r := httptest.NewRequest(http.MethodPost, "/admin/accounts", strings.NewReader(body))
	r.Header.Set("X-Admin-Key", "admin-key")
	w := httptest.NewRecorder()
	admin.adminMux.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model_limits":{"m1":128000}`) {
		t.Fatalf("create response missing model_limits: %s", w.Body.String())
	}

	m, err := store.GetModel(context.Background(), "a/m1")
	if err != nil || m == nil {
		t.Fatalf("m=%+v err=%v", m, err)
	}
	if m.ContextWindow != 128000 {
		t.Fatalf("m1 window=%d, want 128000", m.ContextWindow)
	}
	m2, err := store.GetModel(context.Background(), "a/m2")
	if err != nil || m2 == nil {
		t.Fatalf("m2=%+v err=%v", m2, err)
	}
	if m2.ContextWindow != 0 {
		t.Fatalf("m2 window=%d, want 0", m2.ContextWindow)
	}

	// PUT 不带 model_limits：沿用已配窗口（与 models/headers 同语义）
	r = httptest.NewRequest(http.MethodPut, "/admin/accounts/a", strings.NewReader(`{"type":"api-key","protocol":"openai-chat","base_url":"https://example.test","api_key":"secret","models":{"m1":"n1","m2":"n2"}}`))
	r.Header.Set("X-Admin-Key", "admin-key")
	w = httptest.NewRecorder()
	admin.adminMux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
	}
	m, err = store.GetModel(context.Background(), "a/m1")
	if err != nil || m == nil {
		t.Fatalf("m=%+v err=%v", m, err)
	}
	if m.ContextWindow != 128000 {
		t.Fatalf("window wiped by PUT without model_limits: %d", m.ContextWindow)
	}

	r = httptest.NewRequest(http.MethodGet, "/admin/accounts/a", nil)
	r.Header.Set("X-Admin-Key", "admin-key")
	w = httptest.NewRecorder()
	admin.adminMux.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model_limits":{"m1":128000}`) {
		t.Fatalf("get status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestModelLimitsRejectsNegative(t *testing.T) {
	admin, _ := testAdmin(t)
	body := `{"name":"a","type":"api-key","protocol":"openai-chat","base_url":"https://example.test","api_key":"secret",
		"models":{"m1":"n1"},"model_limits":{"m1":-1}}`
	r := httptest.NewRequest(http.MethodPost, "/admin/accounts", strings.NewReader(body))
	r.Header.Set("X-Admin-Key", "admin-key")
	w := httptest.NewRecorder()
	admin.adminMux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "model_limits") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
