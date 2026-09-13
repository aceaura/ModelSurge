package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"relayd/backend/relaystore"
)

func (s *Server) mountRelayAdmin() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/user-models", s.relayListUserModels)
	mux.HandleFunc("PUT /admin/user-models/{name}", s.relayPutUserModel)
	mux.HandleFunc("DELETE /admin/user-models/{name}", s.relayDeleteUserModel)
	mux.HandleFunc("GET /admin/groups", s.relayListGroups)
	mux.HandleFunc("PUT /admin/groups/{id}", s.relayPutGroup)
	mux.HandleFunc("DELETE /admin/groups/{id}", s.relayDeleteGroup)
	mux.HandleFunc("PUT /admin/groups/{id}/members", s.relayPutMembers)
	mux.HandleFunc("GET /admin/groups/{id}/cache", s.relayInspectCache)
	mux.HandleFunc("DELETE /admin/groups/{id}/cache", s.relayInvalidateCache)
	s.adminMux = mux
}

func decodeAdmin(r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func (s *Server) relayListUserModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.remoteSched.Store.ListUserModels(r.Context())
	if err != nil {
		adminError(w, 500, "", "relay store unavailable")
		return
	}
	writeAdminJSON(w, 200, models)
}

func (s *Server) relayPutUserModel(w http.ResponseWriter, r *http.Request) {
	var in relaystore.UserModel
	if err := decodeAdmin(r, &in); err != nil {
		adminError(w, 400, "", "invalid json")
		return
	}
	in.Name = r.PathValue("name")
	if in.Name == "" || strings.Contains(in.Name, "/") || in.Protocol == "" || in.APIKey == "" {
		adminError(w, 400, "", "name, protocol and api_key are required")
		return
	}
	if err := s.remoteSched.Store.PutUserModel(r.Context(), in); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	in.APIKey = ""
	writeAdminJSON(w, 200, in)
}

func (s *Server) relayDeleteUserModel(w http.ResponseWriter, r *http.Request) {
	if err := s.remoteSched.Store.DeleteUserModel(r.Context(), r.PathValue("name")); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) relayListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.remoteSched.Store.ListGroups(r.Context())
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	writeAdminJSON(w, 200, groups)
}

func (s *Server) relayPutGroup(w http.ResponseWriter, r *http.Request) {
	var in relaystore.Group
	if err := decodeAdmin(r, &in); err != nil {
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
	if err := s.remoteSched.Store.PutGroup(r.Context(), in); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	writeAdminJSON(w, 200, in)
}

func (s *Server) relayDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.remoteSched.Store.DeleteGroup(r.Context(), r.PathValue("id")); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) relayPutMembers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Members []string `json:"members"`
	}
	if err := decodeAdmin(r, &in); err != nil {
		adminError(w, 400, "", "invalid json")
		return
	}
	if err := s.remoteSched.ValidateMembers(r.Context(), in.Members); err != nil {
		adminError(w, 400, "members", err.Error())
		return
	}
	if err := s.remoteSched.Store.AddMembers(r.Context(), r.PathValue("id"), in.Members); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	writeAdminJSON(w, 200, in)
}

func (s *Server) relayInspectCache(w http.ResponseWriter, r *http.Request) {
	groups, err := s.remoteSched.Store.ListGroups(r.Context())
	if err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	for _, g := range groups {
		if g.ID == r.PathValue("id") {
			writeAdminJSON(w, 200, map[string]any{"group_id": g.ID, "upstream_model_id": g.CachedTarget, "last_result": g.LastResult, "updated_at": g.CacheUpdated})
			return
		}
	}
	adminError(w, 404, "id", "group not found")
}

func (s *Server) relayInvalidateCache(w http.ResponseWriter, r *http.Request) {
	if err := s.remoteSched.Store.InvalidateCache(r.Context(), r.PathValue("id")); err != nil {
		adminError(w, 500, "", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
