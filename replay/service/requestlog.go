package service

import (
	"log"
	"net/http"
	"time"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *HTTPServer) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := r.Header.Get("X-Request-ID")
		if s.accessLogEnabled {
			log.Printf("replay phase=request_in request_id=%s method=%s path=%s", requestID, r.Method, r.URL.Path)
		}
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		if s.accessLogEnabled {
			log.Printf("replay phase=response_out request_id=%s method=%s path=%s status=%d bytes=%d latency=%s", requestID, r.Method, r.URL.Path, recorder.status, recorder.bytes, time.Since(started))
		}
	})
}

func requestID(ctxRequestID, headerRequestID string) string {
	if ctxRequestID != "" {
		return ctxRequestID
	}
	return headerRequestID
}

func withRequestID(r *http.Request, id string) *http.Request {
	return r.WithContext(replayv1.WithRequestID(r.Context(), id))
}
