// kiro_auth.go kiro 账号的凭据读取与 token 生命周期（KiroaaS auth.py 的 Go 翻译）。
// 三凭据源：refresh_token 直配 / creds_file JSON / cli_db SQLite；
// 两刷新端点：KIRO_DESKTOP 与 AWS_SSO_OIDC。
// 轮转即持久化（token_state 列），启动优先库内最新 token。
package account

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 认证类型（TokenState.AuthType）。
const (
	AuthTypeKiroDesktop = "KIRO_DESKTOP"
	AuthTypeAWSSSOOIDC  = "AWS_SSO_OIDC"
)

// 凭据源（KiroAccount.Source）。
const (
	SourceRefreshToken = "refresh_token"
	SourceCredsFile    = "creds_file"
	SourceCliDB        = "cli_db"
)

// cli_db auth_kv 的 token key（优先级序）：social > kiro-cli OIDC > 旧版。
var sqliteTokenKeys = []string{
	"kirocli:social:token",
	"kirocli:odic:token",
	"codewhisperer:odic:token",
}

// cli_db 的设备注册 key（AWS SSO OIDC 专用，提供 clientId/clientSecret）。
var sqliteRegistrationKeys = []string{
	"kirocli:odic:device-registration",
	"codewhisperer:odic:device-registration",
}

// tokenRefreshThreshold 过期不足该窗口即预刷新；refreshExpiryBuffer 刷新预留。
const (
	tokenRefreshThreshold = 600 * time.Second
	refreshExpiryBuffer   = 60 * time.Second
	maxErrBody            = 4 * 1024
)

// 刷新端点模板（测试可替换以指向 httptest）。
var (
	kiroRefreshURLTemplate = "https://prod.%s.auth.desktop.kiro.dev/refreshToken"
	awsSSOOIDCURLTemplate  = "https://oidc.%s.amazonaws.com/token"
)

// regionRe 合法区域格式（us-east-1 / eu-central-1 / cn-north-1...）。
var regionRe = regexp.MustCompile(`^[a-z]+(-[a-z]+)+-\d+$`)

// refreshStatusError 刷新端点返回非 2xx。
type refreshStatusError struct {
	status int
	body   string
}

func (e *refreshStatusError) Error() string {
	return fmt.Sprintf("kiro auth: refresh endpoint returned %d: %s", e.status, e.body)
}

// AuthService 单个 kiro 账号的 token 生命周期管理。
// 单账号刷新串行化（mu）；预刷新 + 403 强刷 + 400 降级。
type AuthService struct {
	mu    sync.Mutex
	store *Store // 可空：无持久化（测试）
	name  string // 账号名（SaveTokenState 用）
	k     *KiroAccount

	token             TokenState // 运行时 token 状态
	scopes            []string   // OAuth scopes（cli_db 源回写用）
	ssoRegion         string     // SSO 刷新区（刷新端点用）
	detectedAPIRegion string     // 凭据中检测出的 API 区
	sqliteTokenKey    string     // cli_db 当前 token 所在 key（回写定位）
	clientIDHash      string     // 企业版 IDE 的 clientIdHash

	fingerprint string
	client      *http.Client
	now         func() time.Time

	// fetchProfiles profileArn 自动回填钩子（Manager 注入控制面 client），每实例一次。
	fetchProfiles   func(ctx context.Context) (string, error)
	profilesFetched bool
}

// NewAuthService 构造：k.Token 非空（库内轮转值）优先，凭据源补齐其余字段。
func NewAuthService(store *Store, name string, k *KiroAccount) *AuthService {
	s := &AuthService{
		store:       store,
		name:        name,
		k:           k,
		fingerprint: machineFingerprint(),
		client:      &http.Client{Timeout: 30 * time.Second, Transport: KiroTransport()},
		now:         time.Now,
	}
	if k.Token != nil {
		s.token = *k.Token
	}
	s.loadCredentials()
	return s
}

// machineFingerprint 机器指纹（UA 标识单实例）：sha256("{host}-{user}-kiro-gateway")。
func machineFingerprint() string {
	host, _ := os.Hostname()
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	if host == "" || user == "" {
		return hex32("default-kiro-gateway")
	}
	return hex32(host + "-" + user + "-kiro-gateway")
}

func hex32(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// GetAccessToken 取有效 access token：不足 600s 预刷新；cli_db 源先重载库；
// 刷新 400 且旧 token 未过期 → 降级用旧 token。
func (s *AuthService) GetAccessToken(ctx context.Context) (string, *TokenState, error) {
	s.ensureProfileArn(ctx) // 锁外：内部走网络，会递归取 token
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getAccessTokenLocked(ctx)
}

func (s *AuthService) getAccessTokenLocked(ctx context.Context) (string, *TokenState, error) {
	if s.token.AccessToken != "" && !s.expiringSoon() {
		return s.token.AccessToken, s.snapshot(), nil
	}
	// cli_db 源：kiro-cli 可能已自己刷新过（内存态），刷新前重读
	if s.k.Source == SourceCliDB {
		s.loadCliDB()
		if s.token.AccessToken != "" && !s.expiringSoon() {
			return s.token.AccessToken, s.snapshot(), nil
		}
	}
	if err := s.refreshLocked(ctx); err != nil {
		var re *refreshStatusError
		if errors.As(err, &re) && re.status == 400 && s.token.AccessToken != "" && !s.tokenExpired() {
			log.Printf("kiro auth %s: refresh rejected 400 but old token still valid, degrading until expiry (%s)",
				s.name, s.token.ExpiresAt.Format(time.RFC3339))
			return s.token.AccessToken, s.snapshot(), nil
		}
		return "", nil, err
	}
	return s.token.AccessToken, s.snapshot(), nil
}

// ForceRefresh 强制刷新（403 触发）。
func (s *AuthService) ForceRefresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(ctx)
}

// EffectiveProfileArn 生效的 profileArn（运行态优先，配置兜底）。
func (s *AuthService) EffectiveProfileArn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token.ProfileArn != "" {
		return s.token.ProfileArn
	}
	return s.k.ProfileArn
}

// EffectiveAPIRegion API 区解析链：账号 api_region > KIRO_API_REGION 环境变量
// > 凭据检测（cli_db ARN / creds_file region）> SSO 区 > us-east-1。
// q.amazonaws.com / runtime.kiro.dev 只在特定区存在，与 SSO 区分离。
func (s *AuthService) EffectiveAPIRegion() string {
	if s.k.APIRegion != "" {
		return s.k.APIRegion
	}
	if env := os.Getenv("KIRO_API_REGION"); env != "" {
		return env
	}
	if s.detectedAPIRegion != "" {
		return s.detectedAPIRegion
	}
	if s.ssoRegion != "" {
		return s.ssoRegion
	}
	return "us-east-1"
}

// SSORegion 刷新端点用的 SSO 区。
func (s *AuthService) SSORegion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ssoRegion != "" {
		return s.ssoRegion
	}
	if s.k.Region != "" {
		return s.k.Region
	}
	return "us-east-1"
}

// Fingerprint 机器指纹（请求头用）。
func (s *AuthService) Fingerprint() string { return s.fingerprint }

// SetProfileFetcher 注入 profileArn 自动回填钩子（Manager 装配时调）。
func (s *AuthService) SetProfileFetcher(fn func(ctx context.Context) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetchProfiles = fn
}

// ensureProfileArn profileArn 为空时自动回填（每实例一次）。失败静默——
// 免费账号本就无 profileArn，控制面请求会再报错。
// 自管锁：网络调用在锁外进行（内部走 KiroClient.request 会递归取 token，
// 持锁做 IO 会自死锁）。
func (s *AuthService) ensureProfileArn(ctx context.Context) {
	if s.fetchProfiles == nil {
		return
	}
	s.mu.Lock()
	if s.profilesFetched || s.token.ProfileArn != "" || s.k.ProfileArn != "" {
		s.mu.Unlock()
		return
	}
	s.profilesFetched = true // 先占位，防并发重复拉取
	s.mu.Unlock()

	arn, err := s.fetchProfiles(ctx)
	if err != nil {
		log.Printf("kiro auth %s: auto-fetch profile arn failed: %v", s.name, err)
		return
	}
	if arn == "" {
		return
	}
	s.mu.Lock()
	s.token.ProfileArn = arn
	s.persistLocked()
	s.mu.Unlock()
	log.Printf("kiro auth %s: profile arn auto-fetched", s.name)
}

// expiringSoon 过期不足 600s 或无过期信息。
func (s *AuthService) expiringSoon() bool {
	if s.token.ExpiresAt.IsZero() {
		return true
	}
	return !s.token.ExpiresAt.After(s.now().Add(tokenRefreshThreshold))
}

// tokenExpired 已真正过期（区别于"快过期"：降级判定用）。
func (s *AuthService) tokenExpired() bool {
	if s.token.ExpiresAt.IsZero() {
		return true
	}
	return !s.now().Before(s.token.ExpiresAt)
}

// snapshot 当前 token 状态副本（携带生效区与 profileArn）。
func (s *AuthService) snapshot() *TokenState {
	ts := s.token
	ts.Region = s.ssoRegion
	if ts.ProfileArn == "" {
		ts.ProfileArn = s.k.ProfileArn
	}
	return &ts
}

// loadCredentials 按源读取凭据：已持久化的轮转 token 优先，源补齐
// clientID/clientSecret/区域/scopes 等字段并做 auth 类型检测。
func (s *AuthService) loadCredentials() {
	s.ssoRegion = s.k.Region
	switch s.k.Source {
	case SourceCredsFile:
		s.loadCredsFile()
	case SourceCliDB:
		s.loadCliDB()
	default: // refresh_token 直配
		if s.token.RefreshToken == "" {
			s.token.RefreshToken = s.k.RefreshToken
		}
	}
	if s.token.Region != "" && s.ssoRegion == "" {
		s.ssoRegion = s.token.Region
	}
	if s.ssoRegion == "" {
		s.ssoRegion = "us-east-1"
	}
	s.token.AuthType = AuthTypeKiroDesktop
	if s.token.ClientID != "" && s.token.ClientSecret != "" {
		s.token.AuthType = AuthTypeAWSSSOOIDC
	}
}

// credsFileJSON creds_file 的字段（camelCase；未知字段回写时保留）。
type credsFileJSON struct {
	RefreshToken string `json:"refreshToken"`
	AccessToken  string `json:"accessToken"`
	ProfileArn   string `json:"profileArn"`
	Region       string `json:"region"`
	ExpiresAt    string `json:"expiresAt"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	ClientIDHash string `json:"clientIdHash"`
}

// loadCredsFile 从 JSON 凭据文件读取。企业版 IDE 带 clientIdHash 时，
// clientId/clientSecret 从 ~/.aws/sso/cache/{hash}.json 设备注册文件补齐。
func (s *AuthService) loadCredsFile() {
	b, err := os.ReadFile(expandHome(s.k.CredsFile))
	if err != nil {
		log.Printf("kiro auth %s: creds file %s: %v", s.name, s.k.CredsFile, err)
		return
	}
	var f credsFileJSON
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("kiro auth %s: creds file %s parse: %v", s.name, s.k.CredsFile, err)
		return
	}
	if s.token.RefreshToken == "" {
		s.token.RefreshToken = f.RefreshToken
	}
	if s.token.AccessToken == "" {
		s.token.AccessToken = f.AccessToken
	}
	if s.token.ProfileArn == "" {
		s.token.ProfileArn = f.ProfileArn
	}
	if s.token.ClientID == "" {
		s.token.ClientID = f.ClientID
	}
	if s.token.ClientSecret == "" {
		s.token.ClientSecret = f.ClientSecret
	}
	if s.token.ExpiresAt.IsZero() {
		s.token.ExpiresAt = parseRFC3339(f.ExpiresAt)
	}
	if f.Region != "" {
		s.ssoRegion = f.Region
		s.detectedAPIRegion = f.Region // 可被 KIRO_API_REGION/账号级覆盖
	}
	if f.ClientIDHash != "" {
		s.clientIDHash = f.ClientIDHash
		s.loadEnterpriseRegistration(f.ClientIDHash)
	}
}

// loadEnterpriseRegistration 企业版 IDE 设备注册：~/.aws/sso/cache/{hash}.json。
func (s *AuthService) loadEnterpriseRegistration(hash string) {
	path := filepath.Join(homeDir(), ".aws", "sso", "cache", hash+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("kiro auth %s: enterprise registration %s: %v", s.name, path, err)
		return
	}
	var reg struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(b, &reg); err != nil {
		log.Printf("kiro auth %s: enterprise registration parse: %v", s.name, err)
		return
	}
	if s.token.ClientID == "" {
		s.token.ClientID = reg.ClientID
	}
	if s.token.ClientSecret == "" {
		s.token.ClientSecret = reg.ClientSecret
	}
}

// loadCliDB 从 kiro-cli SQLite 读凭据（auth_kv token key 优先级序 + 设备注册 +
// state 表 profile ARN 区域检测）。db 值覆盖运行态——kiro-cli 重新登录后
// 库内 token 比我们持久化的新。
func (s *AuthService) loadCliDB() {
	db, err := sql.Open("sqlite", "file:"+expandHome(s.k.CliDB)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Printf("kiro auth %s: open cli db: %v", s.name, err)
		return
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Printf("kiro auth %s: cli db %s: %v", s.name, s.k.CliDB, err)
		return
	}

	for _, key := range sqliteTokenKeys {
		var value string
		if err := db.QueryRow(`SELECT value FROM auth_kv WHERE key=?`, key).Scan(&value); err != nil {
			continue
		}
		var t struct {
			AccessToken  string   `json:"access_token"`
			RefreshToken string   `json:"refresh_token"`
			ProfileArn   string   `json:"profile_arn"`
			Region       string   `json:"region"`
			ExpiresAt    string   `json:"expires_at"`
			Scopes       []string `json:"scopes"`
		}
		if err := json.Unmarshal([]byte(value), &t); err != nil {
			continue
		}
		s.sqliteTokenKey = key
		if t.AccessToken != "" {
			s.token.AccessToken = t.AccessToken
		}
		if t.RefreshToken != "" {
			s.token.RefreshToken = t.RefreshToken
		}
		if t.ProfileArn != "" {
			s.token.ProfileArn = t.ProfileArn
		}
		if t.Region != "" {
			s.ssoRegion = t.Region
		}
		if !s.token.ExpiresAt.IsZero() || t.ExpiresAt != "" {
			if et := parseRFC3339(t.ExpiresAt); !et.IsZero() {
				s.token.ExpiresAt = et
			}
		}
		if len(t.Scopes) > 0 {
			s.scopes = t.Scopes
		}
		break
	}

	// 设备注册（AWS SSO OIDC）
	for _, key := range sqliteRegistrationKeys {
		var value string
		if err := db.QueryRow(`SELECT value FROM auth_kv WHERE key=?`, key).Scan(&value); err != nil {
			continue
		}
		var reg struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Region       string `json:"region"`
		}
		if err := json.Unmarshal([]byte(value), &reg); err != nil {
			continue
		}
		if s.token.ClientID == "" {
			s.token.ClientID = reg.ClientID
		}
		if s.token.ClientSecret == "" {
			s.token.ClientSecret = reg.ClientSecret
		}
		if s.ssoRegion == "" && reg.Region != "" {
			s.ssoRegion = reg.Region
		}
		break
	}

	// state 表 profile ARN -> API 区检测（q/runtime 端点只在特定区存在）
	var profileJSON string
	if err := db.QueryRow(`SELECT value FROM state WHERE key='api.codewhisperer.profile'`).Scan(&profileJSON); err == nil {
		var p struct {
			Arn string `json:"arn"`
		}
		if json.Unmarshal([]byte(profileJSON), &p) == nil && p.Arn != "" {
			if s.token.ProfileArn == "" {
				s.token.ProfileArn = p.Arn
			}
			parts := strings.Split(p.Arn, ":") // arn:aws:codewhisperer:REGION:account:profile/id
			if len(parts) >= 4 && regionRe.MatchString(parts[3]) {
				s.detectedAPIRegion = parts[3]
			}
		}
	}
}

// refreshLocked 执行一次刷新（路由到对应端点），成功后持久化 + 回写。
func (s *AuthService) refreshLocked(ctx context.Context) error {
	var err error
	if s.token.AuthType == AuthTypeAWSSSOOIDC {
		err = s.refreshOIDC(ctx)
		// OIDC 400：kiro-cli 重新登录轮转了 token，重载凭据源重试一次
		var re *refreshStatusError
		if errors.As(err, &re) && re.status == 400 && s.k.Source == SourceCliDB {
			log.Printf("kiro auth %s: oidc refresh 400, reloading cli db and retrying", s.name)
			s.loadCliDB()
			err = s.refreshOIDC(ctx)
		}
	} else {
		err = s.refreshDesktop(ctx)
	}
	if err != nil {
		return err
	}
	s.persistLocked()
	s.writeBack()
	return nil
}

// refreshDesktop KIRO_DESKTOP：POST prod.{sso}.auth.desktop.kiro.dev/refreshToken。
func (s *AuthService) refreshDesktop(ctx context.Context) error {
	if s.token.RefreshToken == "" {
		return errors.New("kiro auth: refresh token is not set")
	}
	url := fmt.Sprintf(kiroRefreshURLTemplate, s.SSORegionLocked())
	body, _ := json.Marshal(map[string]string{"refreshToken": s.token.RefreshToken})
	var resp refreshResponse
	if err := s.postJSON(ctx, url, body, "KiroIDE-0.7.45-"+s.fingerprint, &resp); err != nil {
		return err
	}
	if resp.AccessToken == "" {
		return fmt.Errorf("kiro auth: desktop refresh response has no accessToken")
	}
	s.applyRefresh(resp)
	log.Printf("kiro auth %s: token refreshed via Kiro Desktop, expires %s",
		s.name, s.token.ExpiresAt.Format(time.RFC3339))
	return nil
}

// refreshOIDC AWS_SSO_OIDC：POST oidc.{sso}.amazonaws.com/token（JSON camelCase）。
func (s *AuthService) refreshOIDC(ctx context.Context) error {
	if s.token.RefreshToken == "" {
		return errors.New("kiro auth: refresh token is not set")
	}
	if s.token.ClientID == "" || s.token.ClientSecret == "" {
		return errors.New("kiro auth: clientId/clientSecret required for AWS SSO OIDC")
	}
	url := fmt.Sprintf(awsSSOOIDCURLTemplate, s.SSORegionLocked())
	body, _ := json.Marshal(map[string]string{
		"grantType":    "refresh_token",
		"clientId":     s.token.ClientID,
		"clientSecret": s.token.ClientSecret,
		"refreshToken": s.token.RefreshToken,
	})
	var resp refreshResponse
	if err := s.postJSON(ctx, url, body, "", &resp); err != nil {
		return err
	}
	if resp.AccessToken == "" {
		return fmt.Errorf("kiro auth: oidc refresh response has no accessToken")
	}
	s.applyRefresh(resp)
	log.Printf("kiro auth %s: token refreshed via AWS SSO OIDC, expires %s",
		s.name, s.token.ExpiresAt.Format(time.RFC3339))
	return nil
}

// refreshResponse 两端点共用的响应字段（camelCase）。
type refreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	ProfileArn   string `json:"profileArn"`
}

// applyRefresh 应用刷新结果：轮转 refresh token、计算过期（-60s 缓冲）。
func (s *AuthService) applyRefresh(resp refreshResponse) {
	s.token.AccessToken = resp.AccessToken
	if resp.RefreshToken != "" {
		s.token.RefreshToken = resp.RefreshToken // 轮转
	}
	if resp.ProfileArn != "" {
		s.token.ProfileArn = resp.ProfileArn
	}
	expiresIn := resp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	s.token.ExpiresAt = s.now().Add(time.Duration(expiresIn)*time.Second - refreshExpiryBuffer)
}

// postJSON 发刷新请求；ua 非空时带 KiroIDE UA 指纹。
func (s *AuthService) postJSON(ctx context.Context, url string, body []byte, ua string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("kiro auth: refresh request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &refreshStatusError{status: resp.StatusCode, body: excerptStr(string(respBody))}
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("kiro auth: refresh response parse: %w", err)
	}
	return nil
}

// persistLocked 轮转即持久化（重启恢复）。
func (s *AuthService) persistLocked() {
	if s.store == nil || s.name == "" {
		return
	}
	ts := s.snapshot()
	if err := s.store.SaveTokenState(s.name, ts); err != nil {
		log.Printf("kiro auth %s: persist token state: %v", s.name, err)
	}
}

// writeBack 刷新后回写凭据源（creds_file JSON 合并保留未知字段；
// cli_db read-merge-write 只更新 token 字段，尊重 SQLITE_READONLY）。
func (s *AuthService) writeBack() {
	switch s.k.Source {
	case SourceCredsFile:
		s.writeBackCredsFile()
	case SourceCliDB:
		s.writeBackCliDB()
	}
}

func (s *AuthService) writeBackCredsFile() {
	if s.k.CredsFile == "" {
		return
	}
	path := expandHome(s.k.CredsFile)
	data := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &data) // 解析失败则重建（保留不住的本来就没了）
	}
	data["accessToken"] = s.token.AccessToken
	data["refreshToken"] = s.token.RefreshToken
	if !s.token.ExpiresAt.IsZero() {
		data["expiresAt"] = s.token.ExpiresAt.Format(time.RFC3339)
	}
	if arn := s.snapshot().ProfileArn; arn != "" {
		data["profileArn"] = arn
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		log.Printf("kiro auth %s: write back creds file: %v", s.name, err)
	}
}

func (s *AuthService) writeBackCliDB() {
	if s.k.CliDB == "" {
		return
	}
	if sqliteReadonly() {
		return
	}
	db, err := sql.Open("sqlite", "file:"+expandHome(s.k.CliDB)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return
	}
	defer db.Close()

	tryKey := func(key string) bool {
		var value string
		if err := db.QueryRow(`SELECT value FROM auth_kv WHERE key=?`, key).Scan(&value); err != nil {
			return false
		}
		data := map[string]any{}
		if err := json.Unmarshal([]byte(value), &data); err != nil {
			return false // 结构不明不动它
		}
		data["access_token"] = s.token.AccessToken
		data["refresh_token"] = s.token.RefreshToken
		if !s.token.ExpiresAt.IsZero() {
			data["expires_at"] = s.token.ExpiresAt.Format(time.RFC3339)
		}
		data["region"] = s.ssoRegion
		if len(s.scopes) > 0 {
			data["scopes"] = s.scopes
		}
		merged, err := json.Marshal(data)
		if err != nil {
			return false
		}
		res, err := db.Exec(`UPDATE auth_kv SET value=? WHERE key=?`, string(merged), key)
		if err != nil {
			return false
		}
		n, _ := res.RowsAffected()
		return n > 0
	}
	// 先写来源 key，失败再按优先级兜底
	candidates := []string{s.sqliteTokenKey}
	for _, k := range sqliteTokenKeys {
		if k != s.sqliteTokenKey {
			candidates = append(candidates, k)
		}
	}
	for _, key := range candidates {
		if key != "" && tryKey(key) {
			return
		}
	}
}

// sqliteReadonly SQLITE_READONLY=true/1/yes 时禁用 cli_db 回写。
func sqliteReadonly() bool {
	switch strings.ToLower(os.Getenv("SQLITE_READONLY")) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// SSORegionLocked 已持锁时取 SSO 区。
func (s *AuthService) SSORegionLocked() string {
	if s.ssoRegion != "" {
		return s.ssoRegion
	}
	if s.k.Region != "" {
		return s.k.Region
	}
	return "us-east-1"
}

// parseRFC3339 宽松解析 ISO 8601（Z 后缀/纳秒精度），失败返回零值。
func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

// expandHome 展开 ~ 前缀。
func expandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home := homeDir()
	if home == "" {
		return p
	}
	if len(p) == 1 {
		return home
	}
	return filepath.Join(home, p[1:])
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// excerptStr 截取错误文本前 500 字符。
func excerptStr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	if s == "" {
		s = "empty response"
	}
	return s
}
