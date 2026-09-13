package service

import (
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
