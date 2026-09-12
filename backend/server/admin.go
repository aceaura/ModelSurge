// admin.go 管理面 REST API（任务组 8）。
// 挂载于同一 listener 的 /admin 前缀，X-Admin-Key 鉴权（与客户端 api_key
// 相互独立）；scheduler 未启用或未配置 admin key 时不挂载（404）。
// 账号 CRUD 落库后经 Manager.Reconfigure/Remove 热生效；
// 响应凭据一律脱敏（末 4 位）。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"relayd/backend/account"
)

// mountAdmin 挂载管理路由（sched 与 admin key 均就位时）。
func (s *Server) mountAdmin() {
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
func (s *Server) adminAuth(next http.Handler) http.Handler {
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
	Name            string               `json:"name"`
	Type            string               `json:"type"`
	Enabled         *bool                `json:"enabled"`
	Protocol        string               `json:"protocol"`
	BaseURL         string               `json:"base_url"`
	APIKey          string               `json:"api_key"`
	Models          map[string]string    `json:"models"`
	ModelsAllowlist []string             `json:"models_allowlist"`
	Kiro            *account.KiroAccount `json:"kiro"`
}

// validProtocols api-key 型账号可用的上游协议。
var validProtocols = map[string]bool{
	"anthropic": true, "openai-chat": true, "openai-responses": true, "gemini": true,
}

// validate 字段级校验（8.4）。
func (d *accountDTO) validate() error {
	if d.Type != account.TypeAPIKey && d.Type != account.TypeKiro {
		return fmt.Errorf("type: must be %q or %q", account.TypeAPIKey, account.TypeKiro)
	}
	switch d.Type {
	case account.TypeAPIKey:
		if !validProtocols[d.Protocol] {
			return errors.New("protocol: must be one of anthropic/openai-chat/openai-responses/gemini")
		}
		if !strings.HasPrefix(d.BaseURL, "http://") && !strings.HasPrefix(d.BaseURL, "https://") {
			return errors.New("base_url: must start with http:// or https://")
		}
		if d.APIKey == "" {
			return errors.New("api_key: required for api-key accounts")
		}
	case account.TypeKiro:
		if d.Kiro == nil {
			return errors.New("kiro: required for kiro accounts")
		}
		switch d.Kiro.Source {
		case account.SourceRefreshToken:
			if d.Kiro.RefreshToken == "" {
				return errors.New("kiro.refresh_token: required when source is refresh_token")
			}
		case account.SourceCredsFile:
			if d.Kiro.CredsFile == "" {
				return errors.New("kiro.creds_file: required when source is creds_file")
			}
		case account.SourceCliDB:
			if d.Kiro.CliDB == "" {
				return errors.New("kiro.cli_db: required when source is cli_db")
			}
		default:
			return errors.New("kiro.source: must be one of refresh_token/creds_file/cli_db")
		}
	}
	for k, v := range d.Models {
		if k == "" || v == "" {
			return errors.New("models: keys and values must be non-empty")
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
		ModelsAllowlist: d.ModelsAllowlist,
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

// GET /admin/accounts 列表（脱敏）。
func (s *Server) adminListAccounts(w http.ResponseWriter, r *http.Request) {
	accs := s.sched.Status()
	out := make([]account.Account, len(accs))
	for i := range accs {
		out[i] = accs[i].Masked()
	}
	writeAdminJSON(w, 200, out)
}

// POST /admin/accounts 创建：校验 -> 落库 -> Reconfigure（立即可调度）。
func (s *Server) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
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
	writeAdminJSON(w, 201, fresh.Masked())
}

// GET /admin/accounts/{name} 详情（脱敏）。
func (s *Server) adminGetAccount(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) adminUpdateAccount(w http.ResponseWriter, r *http.Request) {
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
	if d.ModelsAllowlist == nil {
		d.ModelsAllowlist = existing.ModelsAllowlist
	}
	if err := d.validate(); err != nil {
		adminError(w, 400, "", err.Error())
		return
	}
	acc := d.toAccount()
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
	writeAdminJSON(w, 200, fresh.Masked())
}

// DELETE /admin/accounts/{name} 删除（移出调度；usage 历史保留）。
func (s *Server) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
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
	w.WriteHeader(204)
}

// POST /admin/accounts/{name}/refresh 强刷 kiro token / 重初始化运行时。
func (s *Server) adminRefreshAccount(w http.ResponseWriter, r *http.Request) {
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
// api-key 对 base_url 发探测请求（任何 HTTP 应答即视为可达）。
func (s *Server) adminTestAccount(w http.ResponseWriter, r *http.Request) {
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
	// api-key：探测 base_url 可达性（不消耗 tokens）
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, acc.BaseURL, nil)
	if err != nil {
		adminError(w, 400, "base_url", err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeAdminJSON(w, 502, map[string]any{
			"error": map[string]any{"type": "upstream_error", "message": "unreachable: " + err.Error()},
		})
		return
	}
	defer resp.Body.Close()
	writeAdminJSON(w, 200, map[string]any{"ok": true, "status": resp.StatusCode})
}

// GET /admin/accounts/{name}/usage 代理 kiro GetUsageLimits（配额画像）。
func (s *Server) adminAccountUsage(w http.ResponseWriter, r *http.Request) {
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

// GET /admin/models 全账号模型并集（api-key 映射键 ∪ kiro 展示列表）。
func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	set := map[string]bool{}
	for _, a := range s.sched.Status() {
		if !a.Enabled || a.Disabled {
			continue
		}
		if a.Type == account.TypeKiro {
			if rt := s.sched.KiroRuntimeOf(a.Name); rt != nil {
				for _, m := range rt.AvailableModels() {
					set[m] = true
				}
			}
			continue
		}
		for m := range a.Models {
			set[m] = true
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	writeAdminJSON(w, 200, map[string]any{"models": out})
}
