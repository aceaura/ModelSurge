package upstream

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	"github.com/aceaura/ModelSurge/upstream/ir"
)

const maxJSON = 1 << 20

type HTTPServer struct {
	Service          *Service
	ServiceKey       string
	AdminHandler     http.Handler
	AccessLogEnabled bool
	mux              *http.ServeMux
}

func NewHTTPServer(service *Service, key string) *HTTPServer {
	s := &HTTPServer{Service: service, ServiceKey: key, AccessLogEnabled: true, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET "+upstreamv1.BasePath+"/health", s.health)
	s.mux.HandleFunc("GET "+upstreamv1.BasePath+"/models", s.models)
	s.mux.HandleFunc("GET "+upstreamv1.BasePath+"/models/", s.resolve)
	s.mux.HandleFunc("POST "+upstreamv1.BasePath+"/candidates/evaluate", s.evaluate)
	s.mux.HandleFunc("POST "+upstreamv1.BasePath+"/results", s.results)
	s.mux.HandleFunc("POST "+upstreamv1.BasePath+"/kiro/execute", s.executeKiro)
	s.mux.HandleFunc("POST "+upstreamv1.BasePath+"/kiro/web-search", s.webSearch)
	return s
}
func (s *HTTPServer) Handler() http.Handler {
	dataPlane := s.accessLog(s.auth(s.mux))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin") && s.AdminHandler != nil {
			s.AdminHandler.ServeHTTP(w, r)
			return
		}
		dataPlane.ServeHTTP(w, r)
	})
}
func (s *HTTPServer) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ServiceKey == "" || r.Header.Get("Authorization") != "Bearer "+s.ServiceKey {
			writeErr(w, http.StatusUnauthorized, upstreamv1.Error{Code: upstreamv1.CodeUnauthorized, Message: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *HTTPServer) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, upstreamv1.HealthResponse{Status: "ready", Database: "ready"})
}
func (s *HTTPServer) models(w http.ResponseWriter, r *http.Request) {
	v, err := s.Service.Models(r.Context())
	if err != nil {
		writeErr(w, 500, *internalError())
		return
	}
	writeJSON(w, 200, upstreamv1.ModelsResponse{Models: v})
}
func (s *HTTPServer) resolve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, upstreamv1.BasePath+"/models/")
	id, ok := strings.CutSuffix(path, "/resolve")
	if !ok || id == "" {
		writeErr(w, 404, upstreamv1.Error{Code: upstreamv1.CodeNotFound, Message: "not found"})
		return
	}
	if s.AccessLogEnabled {
		log.Printf("upstream phase=resolve request_id=%s target=%s", requestIDFrom(r.Context()), id)
	}
	t, e := s.Service.Resolve(r.Context(), id)
	if e != nil {
		status := 503
		if e.Code == upstreamv1.CodeNotFound {
			status = 404
		}
		writeErr(w, status, *e)
		return
	}
	writeJSON(w, 200, t)
}
func (s *HTTPServer) evaluate(w http.ResponseWriter, r *http.Request) {
	var req upstreamv1.EvaluateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, upstreamv1.Error{Code: upstreamv1.CodeInvalidRequest, Message: "invalid request"})
		return
	}
	if s.AccessLogEnabled {
		log.Printf("upstream phase=evaluate request_id=%s target_ids=%d", requestIDFrom(r.Context()), len(req.TargetIDs))
	}
	v, err := s.Service.Evaluate(r.Context(), req.TargetIDs)
	if err != nil {
		writeErr(w, 500, *internalError())
		return
	}
	writeJSON(w, 200, upstreamv1.EvaluateResponse{Candidates: v})
}
func (s *HTTPServer) results(w http.ResponseWriter, r *http.Request) {
	var req upstreamv1.ResultReport
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, upstreamv1.Error{Code: upstreamv1.CodeInvalidRequest, Message: "invalid request"})
		return
	}
	if s.AccessLogEnabled {
		log.Printf("upstream phase=result request_id=%s target=%s outcome=%s status=%d attempt=%d usage_in=%d usage_out=%d cache_read=%d cache_creation=%d", requestIDFrom(r.Context()), req.TargetID, req.Outcome, req.Status, req.Attempt, req.Usage.InputTokens, req.Usage.OutputTokens, req.Usage.CacheRead, req.Usage.CacheCreation)
	}
	result, err := s.Service.Report(r.Context(), req)
	if err != nil {
		writeErr(w, 400, upstreamv1.Error{Code: upstreamv1.CodeInvalidRequest, Message: "invalid report"})
		return
	}
	writeJSON(w, 200, result)
}
func (s *HTTPServer) executeKiro(w http.ResponseWriter, r *http.Request) {
	var req upstreamv1.KiroExecuteRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, upstreamv1.Error{Code: upstreamv1.CodeInvalidRequest, Message: "invalid request", Status: http.StatusBadRequest})
		return
	}
	if req.RequestID == "" {
		req.RequestID = requestIDFrom(r.Context())
	}
	r = r.WithContext(withRequestID(r.Context(), req.RequestID))
	if s.AccessLogEnabled {
		log.Printf("upstream phase=kiro_execute request_id=%s target=%s request_bytes=%d", req.RequestID, req.TargetID, len(req.Request))
	}
	execution, execErr := s.Service.ExecuteKiro(r.Context(), req)
	if execErr != nil {
		status := execErr.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		writeErr(w, status, *execErr)
		return
	}
	defer execution.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(event ir.Event) error {
		if err := json.NewEncoder(w).Encode(event); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	for _, event := range execution.First {
		if emit(event) != nil {
			return
		}
	}
	_ = execution.Continue(emit)
}

func (s *HTTPServer) webSearch(w http.ResponseWriter, r *http.Request) {
	var req upstreamv1.WebSearchRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, upstreamv1.Error{Code: upstreamv1.CodeInvalidRequest, Message: "invalid request"})
		return
	}
	if s.AccessLogEnabled {
		log.Printf("upstream phase=web_search request_id=%s target=%s", requestIDFrom(r.Context()), req.TargetID)
	}
	result, err := s.Service.WebSearch(r.Context(), req)
	if err != nil {
		writeErr(w, 502, upstreamv1.Error{Code: upstreamv1.CodeInternal, Message: "web search failed", Retryable: true})
		return
	}
	writeJSON(w, 200, result)
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, maxJSON))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, status int, e upstreamv1.Error) {
	writeJSON(w, status, upstreamv1.ErrorEnvelope{Error: e})
}
