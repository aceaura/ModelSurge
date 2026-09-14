package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
)

type HTTPServer struct {
	service          *Service
	serviceKey       string
	adminKey         string
	accessLogEnabled bool
	mux              *http.ServeMux
}

func NewHTTPServer(service *Service, serviceKey, adminKey string) *HTTPServer {
	s := &HTTPServer{service: service, serviceKey: serviceKey, adminKey: adminKey, accessLogEnabled: true, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET "+replayv1.BasePath+"/health", s.withServiceAuth(s.health))
	s.mux.HandleFunc("GET "+replayv1.BasePath+"/models", s.withServiceAuth(s.models))
	s.mux.HandleFunc("POST "+replayv1.BasePath+"/dispatch", s.withServiceAuth(s.dispatch))
	s.mux.HandleFunc("POST "+replayv1.BasePath+"/results", s.withServiceAuth(s.results))
	s.mux.HandleFunc("POST "+replayv1.BasePath+"/kiro/execute", s.withServiceAuth(s.executeKiro))
	s.mux.HandleFunc("POST "+replayv1.BasePath+"/kiro/web-search", s.withServiceAuth(s.webSearch))
	s.mountAdmin()
	return s
}

func (s *HTTPServer) Handler() http.Handler { return s.accessLog(s.mux) }

func (s *HTTPServer) SetAccessLog(enabled bool) { s.accessLogEnabled = enabled }

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
	r = withRequestID(r, req.RequestID)
	if s.accessLogEnabled {
		log.Printf("replay phase=dispatch request_id=%s model=%s proto=%s tried=%d", req.RequestID, req.Model, req.InboundProtocol, len(req.TriedIDs))
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
	r = withRequestID(r, report.RequestID)
	if s.accessLogEnabled {
		log.Printf("replay phase=result request_id=%s target=%s outcome=%s status=%d attempt=%d usage_in=%d usage_out=%d cache_read=%d cache_creation=%d", report.RequestID, report.TargetID, report.Outcome, report.Status, report.Attempt, report.Usage.InputTokens, report.Usage.OutputTokens, report.Usage.CacheRead, report.Usage.CacheCreation)
	}
	result, err := s.service.Report(r.Context(), report)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *HTTPServer) executeKiro(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var req replayv1.KiroExecuteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, replayv1.Error{Code: replayv1.CodeInvalidRequest, Message: "invalid json", Status: http.StatusBadRequest})
		return
	}
	req.RequestID = requestID(req.RequestID, r.Header.Get("X-Request-ID"))
	r = withRequestID(r, req.RequestID)
	if s.accessLogEnabled {
		log.Printf("replay phase=kiro_execute_out request_id=%s target=%s request_bytes=%d", req.RequestID, req.TargetID, len(req.Request))
	}
	executor, ok := s.service.Upstream.(interface {
		ExecuteKiro(context.Context, upstreamv1.KiroExecuteRequest) (*http.Response, error)
	})
	if !ok {
		writeError(w, http.StatusBadGateway, replayv1.Error{Code: replayv1.CodeInternal, Message: "upstream execute unavailable", Retryable: true, Status: http.StatusBadGateway})
		return
	}
	resp, err := executor.ExecuteKiro(r.Context(), upstreamv1.KiroExecuteRequest{RequestID: req.RequestID, TargetID: req.TargetID, Request: req.Request})
	if err != nil {
		writeError(w, http.StatusBadGateway, replayv1.Error{Code: replayv1.CodeInternal, Message: err.Error(), Retryable: true, Status: http.StatusBadGateway})
		return
	}
	defer resp.Body.Close()
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if cacheControl := resp.Header.Get("Cache-Control"); cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var bytesWritten int64
	var events int
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			events += strings.Count(string(buf[:n]), "\n")
			written, writeErr := w.Write(buf[:n])
			bytesWritten += int64(written)
			if writeErr != nil {
				if s.accessLogEnabled {
					log.Printf("replay phase=kiro_stream_done request_id=%s status=%d bytes=%d events=%d latency=%s error=true", req.RequestID, resp.StatusCode, bytesWritten, events, time.Since(started))
				}
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if s.accessLogEnabled {
				log.Printf("replay phase=kiro_stream_done request_id=%s status=%d bytes=%d events=%d latency=%s error=%t", req.RequestID, resp.StatusCode, bytesWritten, events, time.Since(started), readErr != io.EOF)
			}
			return
		}
	}
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
