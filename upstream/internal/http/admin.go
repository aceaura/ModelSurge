// admin.go 管理面 REST API（任务组 8）。
// 挂载于同一 listener 的 /admin 前缀，X-Admin-Key 鉴权（与客户端 api_key
// 相互独立）；scheduler 未启用或未配置 admin key 时不挂载（404）。
// 账号 CRUD 落库后经 Manager.Reconfigure/Remove 热生效；
// 响应凭据一律脱敏（末 4 位）。
package upstreamhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/upstream/account"
	"github.com/aceaura/ModelSurge/upstream/ir"
)

// Admin exposes the upstream-owned account administration API.
type Admin struct {
	sched          *account.Manager
	store          *account.Store
	adminKey       string
	adminMux       *http.ServeMux
	probeCl        *http.Client
	accountChanged func() error
}

// NewAccountAdmin creates the authenticated /admin/accounts handler.
func NewAccountAdmin(sched *account.Manager, adminKey string, accountChanged ...func() error) http.Handler {
	s := &Admin{sched: sched, store: sched.Store(), adminKey: adminKey, probeCl: http.DefaultClient}
	if len(accountChanged) > 0 {
		s.accountChanged = accountChanged[0]
	}
	s.mountAdmin()
	return s.adminAuth(s.adminMux)
}

// SetProbeClient replaces the client used by account endpoint probes.
func (s *Admin) SetProbeClient(c *http.Client) { s.probeCl = c }

// mountAdmin 挂载管理路由（sched 与 admin key 均就位时）。
func (s *Admin) mountAdmin() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/accounts", s.adminListAccounts)
	mux.HandleFunc("POST /admin/accounts", s.adminCreateAccount)
	mux.HandleFunc("GET /admin/accounts/{name}", s.adminGetAccount)
	mux.HandleFunc("PUT /admin/accounts/{name}", s.adminUpdateAccount)
	mux.HandleFunc("DELETE /admin/accounts/{name}", s.adminDeleteAccount)
	mux.HandleFunc("POST /admin/accounts/{name}/refresh", s.adminRefreshAccount)
	mux.HandleFunc("POST /admin/accounts/{name}/test", s.adminTestAccount)
	mux.HandleFunc("GET /admin/accounts/{name}/usage", s.adminAccountUsage)
	mux.HandleFunc("GET /admin/models", s.adminModels)
	s.adminMux = mux
}

// adminAuth X-Admin-Key 校验。
func (s *Admin) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-Key") != s.adminKey {
			writeAdminJSON(w, 401, map[string]any{
				"error": map[string]any{"type": "authentication_error", "message": "invalid admin key"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// adminError 字段级错误（400 明细）。
func adminError(w http.ResponseWriter, status int, field, msg string) {
	e := map[string]any{"type": "invalid_request_error", "message": msg}
	if field != "" {
		e["field"] = field
		e["message"] = field + ": " + msg
	}
	writeAdminJSON(w, status, map[string]any{"error": e})
}

// accountDTO 管理面账号输入（凭据全量；enabled 缺省 true；
// json.RawMessage 不用——直接复用 account 字段，Enabled 需三态故包一层）。
type accountDTO struct {
	Name             string               `json:"name"`
	Type             string               `json:"type"`
	Enabled          *bool                `json:"enabled"`
	Protocol         string               `json:"protocol"`
	BaseURL          string               `json:"base_url"`
	APIKey           string               `json:"api_key"`
	Models           map[string]string    `json:"models"`
	Headers          map[string]string    `json:"headers"`
	ModelsAllowlist  []string             `json:"models_allowlist"`
	RequestOverrides *ir.Overrides        `json:"request_overrides"`
	Kiro             *account.KiroAccount `json:"kiro"`
}

// validProtocols api-key 型账号可用的上游协议。
var validProtocols = map[string]bool{
	"anthropic": true, "openai-chat": true, "openai-responses": true, "gemini": true, "codex": true,
}

// validate 字段级校验（8.4）。
func (d *accountDTO) validate() error {
	if d.Type != account.TypeAPIKey && d.Type != account.TypeKiro {
		return fmt.Errorf("type: must be %q or %q", account.TypeAPIKey, account.TypeKiro)
	}
	switch d.Type {
	case account.TypeAPIKey:
		if !validProtocols[d.Protocol] {
			return errors.New("protocol: must be one of anthropic/openai-chat/openai-responses/gemini/codex")
		}
		// base_url 宽容输入（裸域名/带 /v1/带网关前缀均可），能否解析出
		// 协议根地址交由 NormalizeBaseURL 裁决；探测在 handler 内做。
		if _, err := account.NormalizeBaseURL(d.BaseURL); err != nil {
			return fmt.Errorf("base_url: %v", err)
		}
		if d.APIKey == "" {
			return errors.New("api_key: required for api-key accounts")
		}
	case account.TypeKiro:
		if d.Kiro == nil {
			return errors.New("kiro: required for kiro accounts")
		}
		if err := account.ValidateKiroCreds(d.Kiro); err != nil {
			return err
		}
	}
	for k, v := range d.Models {
		if k == "" || v == "" {
			return errors.New("models: keys and values must be non-empty")
		}
	}
	for k := range d.Headers {
		if k == "" {
			return errors.New("headers: keys must be non-empty")
		}
	}
	return nil
}

// toAccount DTO -> Account（enabled 缺省 true）。
func (d *accountDTO) toAccount() *account.Account {
	a := &account.Account{
		Name:            d.Name,
		Type:            d.Type,
		Enabled:         d.Enabled == nil || *d.Enabled,
		Protocol:        d.Protocol,
		BaseURL:         d.BaseURL,
		APIKey:          d.APIKey,
		Models:          d.Models,
		Headers:         d.Headers,
		ModelsAllowlist: d.ModelsAllowlist,
		Overrides:       d.RequestOverrides,
		Kiro:            d.Kiro,
	}
	if a.Models == nil {
		a.Models = map[string]string{}
	}
	return a
}

// decodeDTO 解析请求体（1MB 上限）。
func decodeDTO(r *http.Request) (*accountDTO, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var d accountDTO
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// mergeKiroSecrets 空 kiro 凭据字段沿用库内值（管理面不想轮换时不必回显）。
func mergeKiroSecrets(in, existing *account.KiroAccount) *account.KiroAccount {
	if in == nil || existing == nil {
		return in
	}
	if in.RefreshToken == "" {
		in.RefreshToken = existing.RefreshToken
	}
	if in.CredsFile == "" {
		in.CredsFile = existing.CredsFile
	}
	if in.CliDB == "" {
		in.CliDB = existing.CliDB
	}
	if in.CredsText == "" {
		in.CredsText = existing.CredsText
	}
	if in.CredsB64 == "" {
		in.CredsB64 = existing.CredsB64
	}
	if in.Region == "" {
		in.Region = existing.Region
	}
	if in.APIRegion == "" {
		in.APIRegion = existing.APIRegion
	}
	if in.ProfileArn == "" {
		in.ProfileArn = existing.ProfileArn
	}
	return in
}

// adminAccountResponse 账号响应 + 可选探测报告（probe 仅 api-key 探测时出现）。
type adminAccountResponse struct {
	account.Account
	Probe *account.ProbeReport `json:"probe,omitempty"`
}

// probeAndResolve api-key 账号探测：规范化 base_url 作为兜底值 -> 并发探测
// 候选端点（接收原始输入，内部处理 scheme 补全与本地 http 变体）-> 胜出根
// 地址（resolved / 恰一 auth_failed）返回。探测失败不阻断；kiro 账号不探测。
func (s *Admin) probeAndResolve(ctx context.Context, acc *account.Account) (string, *account.ProbeReport) {
	fallback, err := account.NormalizeBaseURL(acc.BaseURL)
	if err != nil {
		return acc.BaseURL, nil
	}
	probe := account.ProbeEndpoint(ctx, s.probeCl, acc.Protocol, acc.BaseURL, acc.APIKey)
	if probe.ResolvedBaseURL != "" {
		return probe.ResolvedBaseURL, probe
	}
	return fallback, probe
}

// GET /admin/accounts 列表（脱敏）。
func (s *Admin) adminListAccounts(w http.ResponseWriter, r *http.Request) {
	accs := s.sched.Status()
	out := make([]account.Account, len(accs))
	for i := range accs {
		out[i] = accs[i].Masked()
	}
	writeAdminJSON(w, 200, out)
}

// POST /admin/accounts 创建：校验 -> 落库 -> Reconfigure（立即可调度）。
func (s *Admin) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
	d, err := decodeDTO(r)
	if err != nil {
		adminError(w, 400, "", "invalid json: "+err.Error())
		return
	}
	if d.Name == "" || strings.Contains(d.Name, "/") {
		adminError(w, 400, "name", "required and must not contain '/'")
		return
	}
	if err := d.validate(); err != nil {
		adminError(w, 400, "", err.Error())
		return
	}
	acc := d.toAccount()
	var probe *account.ProbeReport
	if acc.Type == account.TypeAPIKey {
		base, pr := s.probeAndResolve(r.Context(), acc)
		acc.BaseURL = base
		probe = pr
	}
	if err := s.store.InsertAccount(acc); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			adminError(w, 409, "name", "account already exists")
			return
		}
		adminError(w, 500, "", err.Error())
		return
	}
	fresh, err := s.store.GetAccount(acc.Name)
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	s.sched.Reconfigure(fresh)
	if s.accountChanged != nil {
		if err := s.accountChanged(); err != nil {
			adminError(w, 500, "", "account saved but model catalog sync failed: "+err.Error())
			return
		}
	}
	writeAdminJSON(w, 201, adminAccountResponse{Account: fresh.Masked(), Probe: probe})
}

// GET /admin/accounts/{name} 详情（脱敏）。
func (s *Admin) adminGetAccount(w http.ResponseWriter, r *http.Request) {
	acc, err := s.store.GetAccount(r.PathValue("name"))
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	if acc == nil {
		adminError(w, 404, "name", "account not found")
		return
	}
	writeAdminJSON(w, 200, acc.Masked())
}

// PUT /admin/accounts/{name} 更新：空凭据沿用旧值；type 不可变；
// 落库 -> 重取（带回 token_state）-> Reconfigure 热生效。
func (s *Admin) adminUpdateAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	existing, err := s.store.GetAccount(name)
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	if existing == nil {
		adminError(w, 404, "name", "account not found")
		return
	}
	d, err := decodeDTO(r)
	if err != nil {
		adminError(w, 400, "", "invalid json: "+err.Error())
		return
	}
	d.Name = name // 路径为准
	// 探测触发判定（merge 前取值）：请求提供了 base_url，或协议发生变化。
	baseURLProvided := d.BaseURL != ""
	protocolProvided := d.Protocol != ""
	if d.Type == "" {
		d.Type = existing.Type // 缺省沿用
	}
	if d.Type != existing.Type {
		adminError(w, 400, "type", "immutable; delete and recreate to change type")
		return
	}
	if d.APIKey == "" {
		d.APIKey = existing.APIKey
	}
	if d.BaseURL == "" {
		d.BaseURL = existing.BaseURL
	}
	if d.Protocol == "" {
		d.Protocol = existing.Protocol
	}
	if d.Kiro != nil {
		d.Kiro = mergeKiroSecrets(d.Kiro, existing.Kiro)
	} else if existing.Kiro != nil {
		d.Kiro = existing.Kiro // 未提交 kiro 段：整体沿用
	}
	if d.Models == nil {
		d.Models = existing.Models
	}
	if d.Headers == nil {
		d.Headers = existing.Headers
	}
	if d.ModelsAllowlist == nil {
		d.ModelsAllowlist = existing.ModelsAllowlist
	}
	if d.RequestOverrides == nil {
		d.RequestOverrides = existing.Overrides
	}
	if err := d.validate(); err != nil {
		adminError(w, 400, "", err.Error())
		return
	}
	acc := d.toAccount()
	var probe *account.ProbeReport
	if acc.Type == account.TypeAPIKey && (baseURLProvided || (protocolProvided && acc.Protocol != existing.Protocol)) {
		base, pr := s.probeAndResolve(r.Context(), acc)
		acc.BaseURL = base
		probe = pr
	}
	if err := s.store.UpdateAccount(acc); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	fresh, err := s.store.GetAccount(name)
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	s.sched.Reconfigure(fresh)
	if s.accountChanged != nil {
		if err := s.accountChanged(); err != nil {
			adminError(w, 500, "", "account saved but model catalog sync failed: "+err.Error())
			return
		}
	}
	writeAdminJSON(w, 200, adminAccountResponse{Account: fresh.Masked(), Probe: probe})
}

// DELETE /admin/accounts/{name} 删除（移出调度；usage 历史保留）。
func (s *Admin) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc, err := s.store.GetAccount(name)
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	if acc == nil {
		adminError(w, 404, "name", "account not found")
		return
	}
	if err := s.store.DeleteAccount(name); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	s.sched.Remove(name)
	if s.accountChanged != nil {
		if err := s.accountChanged(); err != nil {
			adminError(w, 500, "", "account deleted but model catalog sync failed: "+err.Error())
			return
		}
	}
	w.WriteHeader(204)
}

// POST /admin/accounts/{name}/refresh 强刷 kiro token / 重初始化运行时。
func (s *Admin) adminRefreshAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rt := s.sched.KiroRuntimeOf(name)
	if rt == nil {
		adminError(w, 400, "", "not a kiro account (or runtime not initialized)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := rt.Auth.ForceRefresh(ctx); err != nil {
		writeAdminJSON(w, 502, map[string]any{
			"error": map[string]any{"type": "upstream_error", "message": err.Error()},
		})
		return
	}
	writeAdminJSON(w, 200, map[string]any{"ok": true})
}

// POST /admin/accounts/{name}/test 连通性：kiro 拉模型列表；
// api-key 全量端点探测（GET 模型列表，零 token 消耗），胜出根地址
// 写回 base_url 并热生效。
func (s *Admin) adminTestAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc, err := s.store.GetAccount(name)
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	if acc == nil {
		adminError(w, 404, "name", "account not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if acc.Type == account.TypeKiro {
		rt := s.sched.KiroRuntimeOf(name)
		if rt == nil {
			adminError(w, 400, "", "kiro runtime not initialized")
			return
		}
		models, err := rt.Client.ListAvailableModels(ctx)
		if err != nil {
			writeAdminJSON(w, 502, map[string]any{
				"error": map[string]any{"type": "upstream_error", "message": err.Error()},
			})
			return
		}
		writeAdminJSON(w, 200, map[string]any{"ok": true, "models": len(models)})
		return
	}
	// api-key：全量探测候选端点（不消耗 tokens），胜出则修正根地址
	base, probe := s.probeAndResolve(ctx, acc)
	if probe == nil {
		adminError(w, 400, "base_url", "unparseable base_url: "+acc.BaseURL)
		return
	}
	if base != acc.BaseURL {
		acc.BaseURL = base
		if err := s.store.UpdateAccount(acc); err != nil {
			adminError(w, 500, "", err.Error())
			return
		}
		if fresh, ferr := s.store.GetAccount(name); ferr == nil {
			s.sched.Reconfigure(fresh)
		}
	}
	writeAdminJSON(w, 200, map[string]any{"ok": probe.OK, "probe": probe})
}

// GET /admin/accounts/{name}/usage 代理 kiro GetUsageLimits（配额画像）。
func (s *Admin) adminAccountUsage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rt := s.sched.KiroRuntimeOf(name)
	if rt == nil {
		adminError(w, 400, "", "usage limits only available for kiro accounts")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	raw, err := rt.Client.GetUsageLimits(ctx)
	if err != nil {
		writeAdminJSON(w, 502, map[string]any{
			"error": map[string]any{"type": "upstream_error", "message": err.Error()},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

// GET /admin/models 全账号模型并集（Manager.Models：api-key 映射键 ∪ kiro 展示列表）。
func (s *Admin) adminModels(w http.ResponseWriter, r *http.Request) {
	writeAdminJSON(w, 200, map[string]any{"models": s.sched.Models()})
}
