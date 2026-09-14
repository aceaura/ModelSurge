package account

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// authTestEnv 刷新端点 mock + 模板替换。
func authTestEnv(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(func() { srv.Close() })
	oldRefresh, oldOIDC := kiroRefreshURLTemplate, awsSSOOIDCURLTemplate
	kiroRefreshURLTemplate = srv.URL + "/desktop/%s/refreshToken"
	awsSSOOIDCURLTemplate = srv.URL + "/oidc/%s/token"
	t.Cleanup(func() {
		kiroRefreshURLTemplate, awsSSOOIDCURLTemplate = oldRefresh, oldOIDC
	})
	return srv
}

func newAuthStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// refresh_token 直配源：刷新、轮转持久化、重启恢复、预刷新窗口。
func TestAuthService_RefreshTokenSource(t *testing.T) {
	var calls int32
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct{ RefreshToken string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.Contains(r.URL.Path, "/desktop/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if req.RefreshToken == "" {
			w.WriteHeader(400)
			return
		}
		fmt.Fprintf(w, `{"accessToken":"at-%d","refreshToken":"rt-rotated-%d","expiresIn":3600,"profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/p"}`, calls, calls)
	})
	store := newAuthStore(t)
	if err := store.InsertAccount(&Account{
		Name: "k", Type: TypeKiro, Enabled: true,
		Kiro: &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt-initial"},
	}); err != nil {
		t.Fatal(err)
	}
	acc, _ := store.GetAccount("k")

	s := NewAuthService(store, "k", acc.Kiro)
	tok, ts, err := s.GetAccessToken(context.Background())
	if err != nil || tok != "at-1" {
		t.Fatalf("GetAccessToken = %q %v %v", tok, ts, err)
	}
	if ts.RefreshToken != "rt-rotated-1" || ts.ProfileArn == "" || ts.AuthType != AuthTypeKiroDesktop {
		t.Errorf("token state = %+v", ts)
	}
	// 二次调用：未过期不刷新
	if _, _, err := s.GetAccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("refresh calls = %d, want 1 (cached token reused)", calls)
	}

	// 强刷（403 触发）
	if err := s.ForceRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("refresh calls = %d, want 2 after force", calls)
	}

	// 重启恢复：从库读回轮转后的 token，新实例直接可用不刷新
	acc2, _ := store.GetAccount("k")
	if acc2.Kiro.Token == nil || acc2.Kiro.Token.RefreshToken != "rt-rotated-2" {
		t.Fatalf("persisted token = %+v, want rotated rt-rotated-2", acc2.Kiro.Token)
	}
	s2 := NewAuthService(store, "k", acc2.Kiro)
	if _, _, err := s2.GetAccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("refresh calls = %d, want 2 (restart must reuse persisted token)", calls)
	}
}

// 预刷新窗口：过期不足 600s 即触发刷新。
func TestAuthService_PreRefreshWindow(t *testing.T) {
	var calls int32
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, `{"accessToken":"at-fresh","expiresIn":3600}`)
	})
	store := newAuthStore(t)
	k := &KiroAccount{
		Source: SourceRefreshToken, RefreshToken: "rt",
		Token: &TokenState{
			AccessToken: "at-old", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(300 * time.Second), // < 600s 阈值
		},
	}
	s := NewAuthService(store, "k", k)
	tok, _, err := s.GetAccessToken(context.Background())
	if err != nil || tok != "at-fresh" {
		t.Fatalf("tok = %q err = %v, want at-fresh (pre-refresh triggered)", tok, err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("refresh calls = %d, want 1", calls)
	}
	// 599s 后过期 -> 刷新；601s -> 不刷新
	s.token.ExpiresAt = time.Now().Add(599 * time.Second)
	s.token.AccessToken = "at-old"
	_, _, _ = s.GetAccessToken(context.Background())
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("599s window: calls = %d, want 2", calls)
	}
	s.token.ExpiresAt = time.Now().Add(601 * time.Second)
	_, _, _ = s.GetAccessToken(context.Background())
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("601s window: calls = %d, want 2 (no refresh)", calls)
	}
}

// 刷新 400 且旧 token 未过期 → 降级用旧 token；已过期 → 报错。
func TestAuthService_DegradeOn400(t *testing.T) {
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	})
	store := newAuthStore(t)
	k := &KiroAccount{
		Source: SourceRefreshToken, RefreshToken: "rt-stale",
		Token: &TokenState{
			AccessToken: "at-old", RefreshToken: "rt-stale",
			ExpiresAt: time.Now().Add(time.Hour), // 旧 token 未真正过期
		},
	}
	s := NewAuthService(store, "k", k)
	tok, _, err := s.GetAccessToken(context.Background())
	if err != nil || tok != "at-old" {
		t.Fatalf("degrade = %q %v, want old token without error", tok, err)
	}

	// 旧 token 真正过期后，400 必须透出错误
	s.token.ExpiresAt = time.Now().Add(-time.Minute)
	if _, _, err := s.GetAccessToken(context.Background()); err == nil {
		t.Fatal("expired token + failed refresh must error")
	}
}

// creds_file 源：OIDC 类型检测（clientId+clientSecret）走 oidc 端点。
func TestAuthService_CredsFileOIDC(t *testing.T) {
	var body struct {
		GrantType    string `json:"grantType"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		RefreshToken string `json:"refreshToken"`
	}
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/oidc/") {
			w.WriteHeader(404)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"accessToken":"at-oidc","refreshToken":"rt-oidc-new","expiresIn":7200}`)
	})
	dir := t.TempDir()
	creds := filepath.Join(dir, "creds.json")
	os.WriteFile(creds, []byte(`{
		"refreshToken":"rt-oidc","accessToken":"","region":"eu-central-1",
		"clientId":"cid","clientSecret":"secret","expiresAt":""
	}`), 0o600)
	store := newAuthStore(t)
	s := NewAuthService(store, "k", &KiroAccount{Source: SourceCredsFile, CredsFile: creds})
	if s.token.AuthType != AuthTypeAWSSSOOIDC {
		t.Fatalf("auth type = %s, want AWS_SSO_OIDC", s.token.AuthType)
	}
	if s.EffectiveAPIRegion() != "eu-central-1" {
		t.Errorf("api region = %s (detected from creds file), want eu-central-1", s.EffectiveAPIRegion())
	}
	tok, _, err := s.GetAccessToken(context.Background())
	if err != nil || tok != "at-oidc" {
		t.Fatalf("tok = %q err = %v", tok, err)
	}
	if body.GrantType != "refresh_token" || body.ClientID != "cid" || body.ClientSecret != "secret" {
		t.Errorf("oidc request = %+v", body)
	}
	// 回写：creds 文件保留未知字段并更新轮转 token
	b, _ := os.ReadFile(creds)
	var written map[string]any
	if err := json.Unmarshal(b, &written); err != nil {
		t.Fatal(err)
	}
	if written["refreshToken"] != "rt-oidc-new" || written["region"] != "eu-central-1" {
		t.Errorf("write-back = %v", written)
	}
}

// cli_db 源：加载（token key 优先级 + 设备注册 + ARN 区域检测）、
// OIDC 400 重载重试、read-merge-write 回写。
func TestAuthService_CliDBSource(t *testing.T) {
	var calls int32
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(400) // 模拟 kiro-cli 重新登录后旧 token 失效
			fmt.Fprint(w, `{"error":"invalid_request"}`)
			return
		}
		fmt.Fprintf(w, `{"accessToken":"at-cli-%d","expiresIn":3600}`, n)
	})

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data.sqlite3")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE state (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// social token 优先级最高；带未知字段验证回写保留
	socialToken, _ := json.Marshal(map[string]any{
		"access_token": "", "refresh_token": "rt-old",
		"startUrl": "https://example.awsapps.com/start", "provider": "google",
	})
	reg, _ := json.Marshal(map[string]any{
		"client_id": "cid", "client_secret": "secret", "region": "us-east-1",
	})
	if _, err := db.Exec(`INSERT INTO auth_kv (key, value) VALUES (?,?), (?,?)`,
		"kirocli:social:token", string(socialToken),
		"kirocli:odic:device-registration", string(reg)); err != nil {
		t.Fatal(err)
	}
	arnJSON, _ := json.Marshal(map[string]string{
		"arn": "arn:aws:codewhisperer:us-west-2:123:profile/team",
	})
	if _, err := db.Exec(`INSERT INTO state (key, value) VALUES ('api.codewhisperer.profile', ?)`, string(arnJSON)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	store := newAuthStore(t)
	s := NewAuthService(store, "k", &KiroAccount{Source: SourceCliDB, CliDB: dbPath})
	if s.token.RefreshToken != "rt-old" {
		t.Fatalf("refresh token = %q, want rt-old from social key", s.token.RefreshToken)
	}
	if s.token.AuthType != AuthTypeAWSSSOOIDC {
		t.Fatalf("auth type = %s, want AWS_SSO_OIDC (device registration)", s.token.AuthType)
	}
	if s.detectedAPIRegion != "us-west-2" {
		t.Errorf("detected api region = %q, want us-west-2 from profile ARN", s.detectedAPIRegion)
	}
	if arn := s.EffectiveProfileArn(); arn != "arn:aws:codewhisperer:us-west-2:123:profile/team" {
		t.Errorf("profile arn = %q", arn)
	}

	// 模拟 kiro-cli 在 400 后重新登录（库内 token 轮转）
	db2, _ := sql.Open("sqlite", "file:"+dbPath)
	db2.Exec(`UPDATE auth_kv SET value=? WHERE key='kirocli:social:token'`,
		`{"access_token":"","refresh_token":"rt-relogin","startUrl":"https://example.awsapps.com/start","provider":"google"}`)
	db2.Close()

	tok, _, err := s.GetAccessToken(context.Background())
	if err != nil || tok != "at-cli-2" {
		t.Fatalf("tok = %q err = %v, want retry after reload to succeed", tok, err)
	}
	if s.token.RefreshToken != "rt-relogin" {
		t.Errorf("refresh token after reload = %q, want rt-relogin", s.token.RefreshToken)
	}

	// 回写：只更新 token 字段，保留 startUrl/provider
	db3, _ := sql.Open("sqlite", "file:"+dbPath)
	var value string
	db3.QueryRow(`SELECT value FROM auth_kv WHERE key='kirocli:social:token'`).Scan(&value)
	db3.Close()
	var written map[string]any
	if err := json.Unmarshal([]byte(value), &written); err != nil {
		t.Fatal(err)
	}
	if written["refresh_token"] != "rt-oidc-none" && written["access_token"] != "at-cli-2" {
		// OIDC 端点未返回新 refreshToken 时保留原值
		if written["refresh_token"] != "rt-relogin" {
			t.Errorf("write-back refresh_token = %v, want rt-relogin preserved", written["refresh_token"])
		}
	}
	if written["startUrl"] != "https://example.awsapps.com/start" || written["provider"] != "google" {
		t.Errorf("write-back must preserve unknown fields: %v", written)
	}
	if written["access_token"] != "at-cli-2" {
		t.Errorf("write-back access_token = %v, want at-cli-2", written["access_token"])
	}
}

// SQLITE_READONLY=true 时跳过 cli_db 回写。
func TestAuthService_CliDBReadonly(t *testing.T) {
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accessToken":"at-x","expiresIn":3600}`)
	})
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ro.sqlite3")
	db, _ := sql.Open("sqlite", "file:"+dbPath)
	db.Exec(`CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	before := `{"access_token":"old","refresh_token":"rt","region":"us-east-1"}`
	db.Exec(`INSERT INTO auth_kv VALUES ('kirocli:social:token', ?)`, before)
	db.Close()

	t.Setenv("SQLITE_READONLY", "true")
	store := newAuthStore(t)
	s := NewAuthService(store, "k", &KiroAccount{Source: SourceCliDB, CliDB: dbPath})
	// 无 device registration -> KIRO_DESKTOP 端点
	if _, _, err := s.GetAccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	db2, _ := sql.Open("sqlite", "file:"+dbPath)
	var after string
	db2.QueryRow(`SELECT value FROM auth_kv WHERE key='kirocli:social:token'`).Scan(&after)
	db2.Close()
	if after != before {
		t.Errorf("readonly mode must not write back: %q", after)
	}
}
