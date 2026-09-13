// kiro_inline_test.go 内联凭据载体校验矩阵：三选一互斥、base64/魔数/8MB
// 预检、空白剥离。
package account

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestValidateKiroCreds(t *testing.T) {
	credsJSON := `{"refreshToken":"rt-x","clientId":"cid","clientSecret":"sec"}`
	credsB64 := base64.StdEncoding.EncodeToString([]byte(credsJSON))

	cases := []struct {
		name    string
		k       KiroAccount
		wantErr string // 空串 = 期望通过
	}{
		// 互斥矩阵
		{"refresh_token path only", KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt-1"}, ""},
		{"refresh_token text only", KiroAccount{Source: SourceRefreshToken, CredsText: "rt-bare"}, ""},
		{"refresh_token b64 only", KiroAccount{Source: SourceRefreshToken, CredsB64: credsB64}, ""},
		{"creds_file text only", KiroAccount{Source: SourceCredsFile, CredsText: credsJSON}, ""},
		{"creds_file b64 only", KiroAccount{Source: SourceCredsFile, CredsB64: credsB64}, ""},
		{"cli_db path only", KiroAccount{Source: SourceCliDB, CliDB: "/tmp/x.db"}, ""},
		{"conflict path+text", KiroAccount{Source: SourceCredsFile, CredsFile: "c.json", CredsText: credsJSON},
			"credentials carrier conflict"},
		{"conflict text+b64", KiroAccount{Source: SourceRefreshToken, CredsText: "a", CredsB64: credsB64},
			"credentials carrier conflict"},
		{"conflict path+b64", KiroAccount{Source: SourceCliDB, CliDB: "x.db", CredsB64: credsB64},
			"credentials carrier conflict"},
		{"missing carrier", KiroAccount{Source: SourceRefreshToken},
			"kiro.refresh_token: required when source is refresh_token"},
		{"bad source", KiroAccount{Source: "nope"}, "kiro.source: must be one of"},

		// refresh_token 内容形态
		{"refresh_token text JSON ok", KiroAccount{Source: SourceRefreshToken, CredsText: `{"refreshToken":"rt"}`}, ""},
		{"refresh_token text JSON missing key", KiroAccount{Source: SourceRefreshToken, CredsText: `{"foo":1}`},
			`JSON missing "refreshToken" key`},
		{"refresh_token text blank", KiroAccount{Source: SourceRefreshToken, CredsText: "   "},
			"refresh token must be non-empty"},

		// creds_file 内容形态
		{"creds_file text non-JSON", KiroAccount{Source: SourceCredsFile, CredsText: "not json"},
			"not valid JSON"},

		// cli_db 内容形态
		{"cli_db text non-JSON", KiroAccount{Source: SourceCliDB, CredsText: "not json"}, "not valid JSON"},
		{"cli_db b64 non-SQLite", KiroAccount{Source: SourceCliDB, CredsB64: credsB64},
			"not a SQLite database"},

		// base64 形态
		{"invalid base64", KiroAccount{Source: SourceRefreshToken, CredsB64: "!!not-b64!!"},
			"invalid base64"},
		{"empty b64 after strip", KiroAccount{Source: SourceRefreshToken, CredsB64: " \n\t "}, "empty base64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKiroCreds(&tc.k)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want contains %q", err, tc.wantErr)
			}
		})
	}
}

// cli_db b64 形态：真 SQLite 文件过魔数校验；8MB 超限拒绝；空白剥离成功。
func TestValidateKiroCreds_SqliteAndLimits(t *testing.T) {
	db := append([]byte("SQLite format 3\x00"), make([]byte, 100)...)
	if err := ValidateKiroCreds(&KiroAccount{
		Source:   SourceCliDB,
		CredsB64: base64.StdEncoding.EncodeToString(db),
	}); err != nil {
		t.Fatalf("sqlite magic: unexpected error: %v", err)
	}

	// 空白剥离（换行粘贴常见）
	chunked := base64.StdEncoding.EncodeToString([]byte("rt-with\nwhitespace"))
	clean, err := decodeInlineB64(chunked[:20] + "\n" + chunked[20:] + "\r\n  ")
	if err != nil {
		t.Fatalf("strip whitespace: %v", err)
	}
	if string(clean) != "rt-with\nwhitespace" {
		t.Errorf("decoded = %q", clean)
	}

	// 超过 8MB 拒绝
	big := make([]byte, maxInlineCredsBytes+1)
	err = ValidateKiroCreds(&KiroAccount{
		Source:   SourceRefreshToken,
		CredsB64: base64.StdEncoding.EncodeToString(big),
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize err = %v, want exceeds limit", err)
	}
}

// refresh_token 内联四形态：裸串/JSON/b64裸串/b64JSON，刷新走通且轮转
// 持久化后内联字段保持原值（不回写）。
func TestAuthService_InlineRefreshToken(t *testing.T) {
	var calls int32
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var req struct{ RefreshToken string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.RefreshToken == "" {
			t.Errorf("refresh request missing token")
			w.WriteHeader(400)
			return
		}
		fmt.Fprintf(w, `{"accessToken":"at-%d","refreshToken":"rt-rot-%d","expiresIn":3600}`, n, n)
	})
	store := newAuthStore(t)

	for i, k := range []*KiroAccount{
		{Source: SourceRefreshToken, CredsText: "rt-inline-bare"},
		{Source: SourceRefreshToken, CredsText: `{"refreshToken":"rt-inline-json"}`},
		{Source: SourceRefreshToken, CredsB64: base64.StdEncoding.EncodeToString([]byte("rt-b64-bare"))},
		{Source: SourceRefreshToken, CredsB64: base64.StdEncoding.EncodeToString([]byte(`{"refreshToken":"rt-b64-json"}`))},
	} {
		name := "k" + string(rune('0'+i))
		want := []string{"rt-inline-bare", "rt-inline-json", "rt-b64-bare", "rt-b64-json"}[i]
		origText, origB64 := k.CredsText, k.CredsB64
		if err := store.InsertAccount(&Account{Name: name, Type: TypeKiro, Enabled: true, Kiro: k}); err != nil {
			t.Fatal(err)
		}
		s := NewAuthService(store, name, k)
		if s.token.RefreshToken != want {
			t.Errorf("case %d: refresh token = %q, want %q", i, s.token.RefreshToken, want)
			continue
		}
		tok, _, err := s.GetAccessToken(context.Background())
		if err != nil || !strings.HasPrefix(tok, "at-") {
			t.Errorf("case %d: tok = %q err = %v", i, tok, err)
			continue
		}
		// 内联字段不回写：轮转后保持录入原值
		if k.CredsText != origText || k.CredsB64 != origB64 {
			t.Errorf("case %d: inline creds mutated: %q %q", i, k.CredsText, k.CredsB64)
		}
		// 轮转持久化：token_state 列已存最新值（从库读回）
		acc, _ := store.GetAccount(name)
		if acc.Kiro.Token == nil || !strings.HasPrefix(acc.Kiro.Token.RefreshToken, "rt-rot-") {
			t.Errorf("case %d: token state not persisted: %+v", i, acc.Kiro.Token)
		}
	}
}

// creds_file 内联：text/b64 原文走同一解析链（OIDC 检测 + 区域），
// 回写因路径字段为空自然跳过。
func TestAuthService_InlineCredsFile(t *testing.T) {
	var calls int32
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if !strings.Contains(r.URL.Path, "/oidc/") {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"accessToken":"at-inline","refreshToken":"rt-rot","expiresIn":7200}`)
	})
	credsJSON := `{"refreshToken":"rt-oidc-inline","region":"eu-central-1","clientId":"cid","clientSecret":"sec"}`

	for i, k := range []*KiroAccount{
		{Source: SourceCredsFile, CredsText: credsJSON},
		{Source: SourceCredsFile, CredsB64: base64.StdEncoding.EncodeToString([]byte(credsJSON))},
	} {
		origText, origB64 := k.CredsText, k.CredsB64
		s := NewAuthService(newAuthStore(t), "k", k)
		if s.token.RefreshToken != "rt-oidc-inline" {
			t.Errorf("case %d: refresh token = %q", i, s.token.RefreshToken)
		}
		if s.token.AuthType != AuthTypeAWSSSOOIDC {
			t.Errorf("case %d: auth type = %s, want AWS_SSO_OIDC", i, s.token.AuthType)
		}
		if s.EffectiveAPIRegion() != "eu-central-1" {
			t.Errorf("case %d: api region = %s", i, s.EffectiveAPIRegion())
		}
		if _, _, err := s.GetAccessToken(context.Background()); err != nil {
			t.Errorf("case %d: %v", i, err)
		}
		// 内联形态无文件路径：回写守卫直接跳过，字段原值不变
		if k.CredsText != origText || k.CredsB64 != origB64 {
			t.Errorf("case %d: inline creds mutated: %q %q", i, k.CredsText, k.CredsB64)
		}
	}
}

// cli_db 内联：b64 SQLite 走临时文件解析链（token/设备注册/ARN 检测全同
// 路径形态）；text 提取 JSON 走 creds 链。
func TestAuthService_InlineCliDB(t *testing.T) {
	var calls int32
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if !strings.Contains(r.URL.Path, "/oidc/") {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"accessToken":"at-cli-%d","expiresIn":3600}`, n)
	})

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cli.sqlite3")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE auth_kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE state (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	socialToken, _ := json.Marshal(map[string]any{"refresh_token": "rt-cli"})
	reg, _ := json.Marshal(map[string]any{"client_id": "cid", "client_secret": "sec", "region": "us-east-1"})
	if _, err := db.Exec(`INSERT INTO auth_kv (key, value) VALUES (?,?), (?,?)`,
		"kirocli:social:token", string(socialToken),
		"kirocli:odic:device-registration", string(reg)); err != nil {
		t.Fatal(err)
	}
	arnJSON, _ := json.Marshal(map[string]string{"arn": "arn:aws:codewhisperer:us-west-2:1:profile/p"})
	if _, err := db.Exec(`INSERT INTO state (key, value) VALUES ('api.codewhisperer.profile', ?)`, string(arnJSON)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// b64 形态：临时文件链
	sb64 := NewAuthService(newAuthStore(t), "k", &KiroAccount{
		Source: SourceCliDB, CredsB64: base64.StdEncoding.EncodeToString(raw),
	})
	if sb64.token.RefreshToken != "rt-cli" || sb64.token.AuthType != AuthTypeAWSSSOOIDC {
		t.Fatalf("b64 load: token = %q auth = %s", sb64.token.RefreshToken, sb64.token.AuthType)
	}
	if sb64.detectedAPIRegion != "us-west-2" {
		t.Errorf("b64 load: detected api region = %q", sb64.detectedAPIRegion)
	}
	if _, _, err := sb64.GetAccessToken(context.Background()); err != nil {
		t.Fatalf("b64 refresh: %v", err)
	}

	// text 形态：提取凭据 JSON 走 creds 链（OIDC 检测）
	stext := NewAuthService(newAuthStore(t), "k", &KiroAccount{
		Source: SourceCliDB, CredsText: `{"refreshToken":"rt-extracted","clientId":"cid","clientSecret":"sec"}`,
	})
	if stext.token.RefreshToken != "rt-extracted" || stext.token.AuthType != AuthTypeAWSSSOOIDC {
		t.Fatalf("text load: token = %q auth = %s", stext.token.RefreshToken, stext.token.AuthType)
	}
	if _, _, err := stext.GetAccessToken(context.Background()); err != nil {
		t.Fatalf("text refresh: %v", err)
	}
}
