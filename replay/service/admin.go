package service

import (
	"log"
	"net/http"
	"strings"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/replay/relaystore"
)

func (s *HTTPServer) mountAdmin() {
	s.mux.HandleFunc("GET /admin/user-models", s.withAdminAuth(s.listUserModels))
	s.mux.HandleFunc("PUT /admin/user-models/{name}", s.withAdminAuth(s.putUserModel))
	s.mux.HandleFunc("DELETE /admin/user-models/{name}", s.withAdminAuth(s.deleteUserModel))
	s.mux.HandleFunc("GET /admin/groups", s.withAdminAuth(s.listGroups))
	s.mux.HandleFunc("PUT /admin/groups/{id}", s.withAdminAuth(s.putGroup))
	s.mux.HandleFunc("DELETE /admin/groups/{id}", s.withAdminAuth(s.deleteGroup))
	s.mux.HandleFunc("PUT /admin/groups/{id}/members", s.withAdminAuth(s.putMembers))
	s.mux.HandleFunc("GET /admin/groups/{id}/cache", s.withAdminAuth(s.inspectCache))
	s.mux.HandleFunc("DELETE /admin/groups/{id}/cache", s.withAdminAuth(s.invalidateCache))
}

func (s *HTTPServer) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !secureEqual(r.Header.Get("X-Admin-Key"), s.adminKey) {
			writeError(w, http.StatusUnauthorized, replayv1.Error{Code: replayv1.CodeUnauthorized, Message: "invalid admin key"})
			return
		}
		next(w, r)
	}
}

func (s *HTTPServer) listUserModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.service.Store.ListUserModels(r.Context())
	if err != nil {
		adminError(w, 500, "", "replay store unavailable")
		return
	}
	writeJSON(w, 200, models)
}

func (s *HTTPServer) putUserModel(w http.ResponseWriter, r *http.Request) {
	var in relaystore.UserModel
	if err := decodeJSON(w, r, &in); err != nil {
		adminError(w, 400, "", "invalid json")
		return
	}
	in.Name = r.PathValue("name")
	if in.Name == "" || strings.Contains(in.Name, "/") || in.Protocol == "" || in.APIKey == "" {
		adminError(w, 400, "", "name, protocol and api_key are required")
		return
	}
	if in.CompressModel != "" {
		if in.CompressModel == in.Name {
			adminError(w, 400, "compress_model", "compress_model must reference a different user model")
			return
		}
		models, err := s.service.Store.ListUserModels(r.Context())
		if err != nil {
			adminError(w, 500, "", err.Error())
			return
		}
		found := false
		for _, m := range models {
			if m.Name == in.CompressModel {
				found = true
				if !m.Enabled {
					adminError(w, 400, "compress_model", "compress_model references a disabled user model")
					return
				}
				break
			}
		}
		if !found {
			adminError(w, 400, "compress_model", "compress_model references an unknown user model")
			return
		}
		s.warnCompressWindow(r, in.Name, in.CompressModel)
	}
	if err := s.service.Store.PutUserModel(r.Context(), in); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	in.APIKey = ""
	writeJSON(w, 200, in)
}

func (s *HTTPServer) deleteUserModel(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Store.DeleteUserModel(r.Context(), r.PathValue("name")); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.service.Store.ListGroups(r.Context())
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	writeJSON(w, 200, groups)
}

func (s *HTTPServer) putGroup(w http.ResponseWriter, r *http.Request) {
	var in relaystore.Group
	if err := decodeJSON(w, r, &in); err != nil {
		adminError(w, 400, "", "invalid json")
		return
	}
	in.ID = r.PathValue("id")
	if in.ID == "" || in.UserModel == "" || in.PolicyType == "" {
		adminError(w, 400, "", "id, user_model and policy_type are required")
		return
	}
	if in.PolicyType == "dynamic" {
		adminError(w, 422, "policy_type", "dynamic policy is unsupported")
		return
	}
	if in.PolicyConfig == "" {
		in.PolicyConfig = "{}"
	}
	if err := s.service.Store.PutGroup(r.Context(), in); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	writeJSON(w, 200, in)
}

func (s *HTTPServer) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Store.DeleteGroup(r.Context(), r.PathValue("id")); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) putMembers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Members []string `json:"members"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		adminError(w, 400, "", "invalid json")
		return
	}
	if err := s.service.Scheduler.ValidateMembers(r.Context(), in.Members); err != nil {
		adminError(w, 400, "members", err.Error())
		return
	}
	if err := s.service.Store.AddMembers(r.Context(), r.PathValue("id"), in.Members); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	writeJSON(w, 200, in)
}

func (s *HTTPServer) inspectCache(w http.ResponseWriter, r *http.Request) {
	groups, err := s.service.Store.ListGroups(r.Context())
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	for _, group := range groups {
		if group.ID == r.PathValue("id") {
			writeJSON(w, 200, map[string]any{"group_id": group.ID, "upstream_model_id": group.CachedTarget, "last_result": group.LastResult, "updated_at": group.CacheUpdated})
			return
		}
	}
	adminError(w, 404, "id", "group not found")
}

func (s *HTTPServer) invalidateCache(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Store.InvalidateCache(r.Context(), r.PathValue("id")); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func adminError(w http.ResponseWriter, status int, field, message string) {
	writeError(w, status, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: message, Field: field})
}

// warnCompressWindow compress_model 窗口 best-effort 告警（设计 3.3）：
// 两个 user model 组首个成员的评估窗口可得且压缩模型窗口不大于原模型时
// 记告警（不阻止保存——窗口是运行时过滤信号，跨库硬校验违反架构不变量）；
// 窗口不可得或评估失败时记说明日志。无组/无成员/无压缩配置时不打扰。
func (s *HTTPServer) warnCompressWindow(r *http.Request, name, compressModel string) {
	firstWindow := func(model string) (int, bool) {
		g, err := s.service.Store.GroupForModel(r.Context(), model)
		if err != nil || g == nil || len(g.Members) == 0 {
			return 0, false
		}
		evals, err := s.service.Upstream.Evaluate(r.Context(), []string{g.Members[0]})
		if err != nil || len(evals) == 0 || evals[0].ContextWindow <= 0 {
			return 0, false
		}
		return evals[0].ContextWindow, true
	}
	origW, origOK := firstWindow(name)
	compW, compOK := firstWindow(compressModel)
	if origOK && compOK {
		if compW <= origW {
			log.Printf("replay admin: user model %s compress_model %s window %d not larger than original %d; compression may not help oversized requests", name, compressModel, compW, origW)
		}
		return
	}
	log.Printf("replay admin: user model %s compress_model %s window unknown (original=%t compress=%t); window check skipped, runtime probing is the backstop", name, compressModel, origOK, compOK)
}
