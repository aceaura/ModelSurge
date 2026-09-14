package account

import (
	"database/sql"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// v1 库（旧 schema）迁移到 v2：数据无损、新列取默认值。
func TestMigrateV1ToV2(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v1.db")

	// 手工建一个 v1 库并写入一行
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(DELETE)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE accounts (
		name           TEXT PRIMARY KEY,
		protocol       TEXT NOT NULL,
		base_url       TEXT NOT NULL,
		api_key        TEXT NOT NULL,
		models         TEXT NOT NULL DEFAULT '{}',
		disabled       INTEGER NOT NULL DEFAULT 0,
		limit_kind     TEXT NOT NULL DEFAULT '',
		cooldown_until INTEGER NOT NULL DEFAULT 0,
		updated_at     INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	cool := time.Now().Add(time.Hour).Unix()
	if _, err := raw.Exec(`INSERT INTO accounts
		(name, protocol, base_url, api_key, models, disabled, limit_kind, cooldown_until, updated_at)
		VALUES ('old','anthropic','http://x','k1','{"m":"m"}',1,'5h',?,123)`, cool); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// 正常 Open：建表 no-op + v2 迁移补列
	store, err := Open(dialect.SQLite, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	accs, err := store.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accs))
	}
	a := accs[0]
	if a.Name != "old" || a.Type != TypeAPIKey || !a.Enabled {
		t.Errorf("v1 row defaults: type=%q enabled=%v, want api-key/true", a.Type, a.Enabled)
	}
	if !a.Disabled || a.LimitKind != "5h" || !a.CooldownUntil.Equal(time.Unix(cool, 0)) {
		t.Errorf("v1 state lost: disabled=%v limit=%q cooldown=%v", a.Disabled, a.LimitKind, a.CooldownUntil)
	}
	if n, ok := a.Serving("m"); !ok || n != "m" {
		t.Errorf("models map lost after migration")
	}

	// 迁移后的库上能插入 kiro 账号（新列就位）
	if err := store.InsertAccount(&Account{
		Name: "k1", Type: TypeKiro, Enabled: true,
		Kiro: &KiroAccount{Source: "refresh_token", RefreshToken: "rt-1"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAccount("k1")
	if err != nil || got == nil {
		t.Fatalf("GetAccount after migration: %v %v", got, err)
	}
	if got.Kiro == nil || got.Kiro.Source != "refresh_token" {
		t.Errorf("kiro account lost kiro fields: %+v", got.Kiro)
	}
}

// CRUD 全流程：插入、读取、更新、token 状态、统计、熔断、删除。
func TestAccountCRUD(t *testing.T) {
	store, err := Open(dialect.SQLite, filepath.Join(t.TempDir(), "crud.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 插入 kiro 账号
	in := &Account{
		Name: "kiro-a", Type: TypeKiro, Enabled: true,
		ModelsAllowlist: []string{"claude-sonnet-4-5", "claude-opus-4-1"},
		Kiro:            &KiroAccount{Source: "creds_file", CredsFile: "C:/creds.json", APIRegion: "us-west-2"},
	}
	if err := store.InsertAccount(in); err != nil {
		t.Fatal(err)
	}

	// 内联凭据账号 round-trip（无迁移，kiro JSON 列直存）
	inline := &Account{
		Name: "kiro-inline", Type: TypeKiro, Enabled: true,
		Kiro: &KiroAccount{
			Source:    "creds_file",
			CredsText: `{"refreshToken":"rt-inline","clientId":"cid"}`,
		},
	}
	if err := store.InsertAccount(inline); err != nil {
		t.Fatal(err)
	}
	gotInline, err := store.GetAccount("kiro-inline")
	if err != nil {
		t.Fatal(err)
	}
	if gotInline.Kiro == nil ||
		gotInline.Kiro.CredsText != inline.Kiro.CredsText ||
		gotInline.Kiro.Source != "creds_file" {
		t.Errorf("inline creds round-trip = %+v", gotInline.Kiro)
	}
	// 掩码：内联字段末 4 位，不留全文
	masked := gotInline.Masked()
	if masked.Kiro.CredsText != `****id"}` {
		t.Errorf("inline creds not masked: %q", masked.Kiro.CredsText)
	}

	// token 状态轮转持久化 + 重启恢复
	ts := &TokenState{
		AccessToken: "at-1", RefreshToken: "rt-rotated",
		ExpiresAt: time.Now().Add(time.Hour),
		AuthType:  "KIRO_DESKTOP", Region: "us-east-1",
	}
	if err := store.SaveTokenState("kiro-a", ts); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAccount("kiro-a")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Kiro.Token == nil || got.Kiro.Token.AccessToken != "at-1" ||
		got.Kiro.Token.RefreshToken != "rt-rotated" || got.Kiro.Token.AuthType != "KIRO_DESKTOP" {
		t.Errorf("token state round-trip = %+v", got.Kiro.Token)
	}
	if got.Kiro.CredsFile != "C:/creds.json" || got.Kiro.APIRegion != "us-west-2" {
		t.Errorf("kiro identity lost: %+v", got.Kiro)
	}
	if len(got.ModelsAllowlist) != 2 {
		t.Errorf("allowlist round-trip = %v", got.ModelsAllowlist)
	}

	// 统计与熔断持久化
	if err := store.SaveStats("kiro-a", AccountStats{Requests: 10, Successes: 9, Failures: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetFailures("kiro-a", 3, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetAccount("kiro-a")
	if got.Stats.Requests != 10 || got.Stats.Successes != 9 {
		t.Errorf("stats round-trip = %+v", got.Stats)
	}
	if got.Failures != 3 || got.LastFailure.Unix() != 1700000000 {
		t.Errorf("failures round-trip = %d %v", got.Failures, got.LastFailure)
	}

	// 更新身份字段：token 状态不丢
	in.Kiro.APIRegion = "eu-central-1"
	in.Enabled = false
	if err := store.UpdateAccount(in); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetAccount("kiro-a")
	if got.Kiro.APIRegion != "eu-central-1" || got.Enabled {
		t.Errorf("update failed: api_region=%q enabled=%v", got.Kiro.APIRegion, got.Enabled)
	}
	if got.Kiro.Token == nil || got.Kiro.Token.AccessToken != "at-1" {
		t.Errorf("token state must survive identity update")
	}

	// 删除
	if err := store.DeleteAccount("kiro-a"); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetAccount("kiro-a")
	if err != nil || got != nil {
		t.Errorf("GetAccount after delete = %v %v, want nil", got, err)
	}
}

// Serving：白名单优先拦截；kiro 型走运行时解析；api-key 型沿用 models 映射。
func TestServingV2(t *testing.T) {
	// api-key + 白名单
	a := &Account{
		Type: TypeAPIKey, Enabled: true,
		Models:          map[string]string{"m1": "n1", "m2": "n2"},
		ModelsAllowlist: []string{"m1"},
	}
	if n, ok := a.Serving("m1"); !ok || n != "n1" {
		t.Errorf("allowlisted m1 = %v %q, want true n1", ok, n)
	}
	if _, ok := a.Serving("m2"); ok {
		t.Errorf("m2 not in allowlist but served")
	}

	// kiro：未注入解析器时透传
	k := &Account{Type: TypeKiro, Enabled: true}
	if n, ok := k.Serving("claude-sonnet-4-5"); !ok || n != "claude-sonnet-4-5" {
		t.Errorf("kiro passthrough = %v %q", ok, n)
	}
	// kiro：注入解析器后按缓存判定
	k.ResolveModel = func(model string) (string, bool) {
		if model == "claude-sonnet-4-5" {
			return "anthropic.claude-sonnet-4-5", true
		}
		return "", false
	}
	if n, ok := k.Serving("claude-sonnet-4-5"); !ok || n != "anthropic.claude-sonnet-4-5" {
		t.Errorf("kiro resolved = %v %q", ok, n)
	}
	if _, ok := k.Serving("gpt-9"); ok {
		t.Errorf("unknown model served without cache entry")
	}
}

// 脱敏：管理 API 输出凭据只保留末 4 位。
func TestMasked(t *testing.T) {
	a := &Account{
		Name: "x", Type: TypeKiro, Enabled: true, APIKey: "sk-1234567890abcd",
		Kiro: &KiroAccount{
			Source: "refresh_token", RefreshToken: "very-long-refresh-token-xyz",
			Token: &TokenState{
				AccessToken: "at-secret-9999", RefreshToken: "rt-secret-8888",
				ClientID: "cid", ClientSecret: "cs-secret-7777",
			},
		},
	}
	m := a.Masked()
	if m.APIKey != "****abcd" {
		t.Errorf("api_key = %q", m.APIKey)
	}
	if m.Kiro.RefreshToken != "****-xyz" {
		t.Errorf("refresh_token = %q", m.Kiro.RefreshToken)
	}
	if m.Kiro.Token.AccessToken != "****9999" || m.Kiro.Token.ClientSecret != "****7777" {
		t.Errorf("token masked = %q %q", m.Kiro.Token.AccessToken, m.Kiro.Token.ClientSecret)
	}
	// 非凭据字段不遮蔽
	if m.Kiro.Token.ClientID != "cid" {
		t.Errorf("client_id should not be masked: %q", m.Kiro.Token.ClientID)
	}
	// 原账号不受影响
	if a.APIKey != "sk-1234567890abcd" {
		t.Errorf("masked leaked into original")
	}
	// 空串保持空
	empty := &Account{Type: TypeKiro}
	if empty.Masked().APIKey != "" {
		t.Errorf("empty secret should stay empty")
	}
}
