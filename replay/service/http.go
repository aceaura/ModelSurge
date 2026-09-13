package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

type HTTPServer struct {
	service    *Service
	serviceKey string
	adminKey   string
	mux        *http.ServeMux
}

func NewHTTPServer(service *Service, serviceKey, adminKey string) *HTTPServer {
	s := &HTTPServer{service: service, serviceKey: serviceKey, adminKey: adminKey, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET "+replayv1.BasePath+"/health", s.withServiceAuth(s.health))
	s.mux.HandleFunc("GET "+replayv1.BasePath+"/models", s.withServiceAuth(s.models))
	s.mux.HandleFunc("POST "+replayv1.BasePath+"/dispatch", s.withServiceAuth(s.dispatch))
	s.mux.HandleFunc("POST "+replayv1.BasePath+"/results", s.withServiceAuth(s.results))
	s.mux.HandleFunc("POST "+replayv1.BasePath+"/kiro/web-search", s.withServiceAuth(s.webSearch))
	s.mountAdmin()
	return s
}

func (s *HTTPServer) Handler() http.Handler { return s.mux }

func (s *HTTPServer) withServiceAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !secureEqual(got, s.serviceKey) {
			writeError(w, http.StatusUnauthorized, replayv1.Error{Code: replayv1.CodeUnauthorized, Message: "invalid service key"})
			return
		}
		next(w, r)
	}
}

func secureEqual(got, want string) bool {
	return want != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *HTTPServer) health(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Store.DB.PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, replayv1.Error{Code: replayv1.CodeInternal, Message: "database unavailable", Retryable: true})
		return
	}
	upstreamStatus := "unknown"
	if h, ok := s.service.Upstream.(interface{ Health(context.Context) error }); ok {
		if err := h.Health(r.Context()); err == nil {
			upstreamStatus = "ok"
		} else {
			upstreamStatus = "unavailable"
		}
	}
	writeJSON(w, http.StatusOK, replayv1.HealthResponse{Status: "ok", Database: "ok", Upstream: upstreamStatus})
}

func (s *HTTPServer) models(w http.ResponseWriter, r *http.Request) {
	models, err := s.service.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, replayv1.Error{Code: replayv1.CodeInternal, Message: "replay store unavailable", Retryable: true})
		return
	}
	writeJSON(w, http.StatusOK, replayv1.ModelsResponse{Models: models})
}

func (s *HTTPServer) dispatch(w http.ResponseWriter, r *http.Request) {
	var req replayv1.DispatchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: "invalid json"})
		return
	}
	lease, err := s.service.Dispatch(r.Context(), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *HTTPServer) results(w http.ResponseWriter, r *http.Request) {
	var report replayv1.ResultReport
	if err := decodeJSON(w, r, &report); err != nil {
		writeError(w, http.StatusBadRequest, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: "invalid json"})
		return
	}
	result, err := s.service.Report(r.Context(), report)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *HTTPServer) webSearch(w http.ResponseWriter, r *http.Request) {
	var req replayv1.WebSearchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: "invalid json"})
		return
	}
	result, err := s.service.WebSearch(r.Context(), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

func writeServiceError(w http.ResponseWriter, err error) {
	var typed replayv1.Error
	if errors.As(err, &typed) {
		status := http.StatusBadRequest
		switch typed.Code {
		case replayv1.CodeUnauthorized:
			status = http.StatusUnauthorized
		case replayv1.CodeNotFound:
			status = http.StatusNotFound
		case replayv1.CodeTargetUnavailable:
			status = http.StatusServiceUnavailable
		case replayv1.CodeConflict:
			status = http.StatusConflict
		}
		writeError(w, status, typed)
		return
	}
	writeError(w, http.StatusBadGateway, replayv1.Error{Code: replayv1.CodeInternal, Message: err.Error(), Retryable: true})
}

func writeError(w http.ResponseWriter, status int, err replayv1.Error) {
	writeJSON(w, status, replayv1.ErrorEnvelope{Error: err})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
