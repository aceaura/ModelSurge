package ingress

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"relayd/backend/catalog"
	"relayd/backend/egress"
	"relayd/strategy"
)

type Handler struct {
	Reg        *strategy.Registry
	Catalog    *catalog.Catalog
	Fwd        *egress.Forwarder
	Tokens     map[string]bool
	MaxBody    int64
	MaxRetries int
	Logger     *slog.Logger
}

func (h *Handler) authOK(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	key := strings.TrimPrefix(auth, "Bearer ")
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	return h.Tokens[key]
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, `{"error":{"message":"`+strings.ReplaceAll(msg, `"`, `'`)+`"}}`)
}

// Mux returns the business-port mux.
func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", h.withAuth(h.serveOpenAI))
	mux.HandleFunc("/v1/messages", h.withAuth(h.serveClaude))
	mux.HandleFunc("/v1/models", h.withAuth(h.serveModels))
	return mux
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authOK(r) {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r)
	}
}

func (h *Handler) serveOpenAI(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, "openai")
}

func (h *Handler) serveClaude(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, "claude")
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, clientProtocol string) {
	clientFmt, err := egress.FormatFor(clientProtocol)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	body, replayable := h.readBody(w, r)
	if body == nil && !replayable {
		return // response already written
	}

	rawModel := gjson.GetBytes(body, "model").String()
	canonical, ok := h.Catalog.Canonicalize(rawModel)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown model: "+rawModel)
		return
	}
	isStream := gjson.GetBytes(body, "stream").Bool()

	tried := map[*strategy.Upstream]bool{}
	maxAttempts := h.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if !replayable {
		maxAttempts = 1
	}

	for range maxAttempts {
		u := h.Reg.Pick(canonical, tried)
		if u == nil {
			break
		}
		tried[u] = true

		native, ok := h.Catalog.NativeFor(u, canonical)
		if !ok {
			continue
		}
		upstreamFmt, err := egress.FormatFor(u.Protocol)
		if err != nil {
			continue
		}

		outBody, err := egress.ConvertRequestBody(r.Context(), clientFmt, upstreamFmt, canonical, native, body, isStream, h.logConv(u.Name))
		if err != nil {
			writeError(w, http.StatusBadRequest, "request conversion failed: "+err.Error())
			return
		}

		start := time.Now()
		resp, err := h.Fwd.Forward(r.Context(), u, outBody, r.Header)
		if err != nil {
			u.Circuit.ReleaseHalfOpen()
			u.Feed(strategy.Signal{Kind: strategy.KindTraffic, At: time.Now(), OK: false, Reason: "forward: " + err.Error(), Latency: time.Since(start)})
			continue
		}

		if resp.StatusCode < 400 {
			u.Feed(strategy.Signal{Kind: strategy.KindTraffic, At: time.Now(), OK: true, Latency: time.Since(start)})
			egress.CopySuccessHeaders(w, resp)
			if isStream {
				if w.Header().Get("Content-Type") == "" {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(resp.StatusCode)
				_ = egress.StreamConvert(r.Context(), upstreamFmt, clientFmt, canonical, resp.Body, w, h.logConv(u.Name))
				resp.Body.Close()
				u.Circuit.ReleaseHalfOpen()
				return
			}
			respBody, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBody()+1))
			resp.Body.Close()
			if err != nil {
				u.Circuit.ReleaseHalfOpen()
				writeError(w, http.StatusBadGateway, "read upstream response: "+err.Error())
				return
			}
			out, err := egress.ConvertResponseBody(r.Context(), upstreamFmt, clientFmt, canonical, respBody, h.logConv(u.Name))
			u.Circuit.ReleaseHalfOpen()
			if err != nil {
				writeError(w, http.StatusBadGateway, "response conversion failed: "+err.Error())
				return
			}
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(resp.StatusCode)
			w.Write(out)
			return
		}

		errBody := egress.ReadErrorBody(resp)
		sig := strategy.FromResponse(resp.StatusCode, resp.Header, errBody)
		sig.At = time.Now()
		sig.Latency = time.Since(start)
		u.Feed(sig)
		u.Circuit.ReleaseHalfOpen()

		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(errBody)
			return
		}
	}

	writeError(w, http.StatusServiceUnavailable, "all upstreams exhausted")
}

func (h *Handler) logConv(upstream string) egress.ConversionLogger {
	return func(msg string, kv ...any) {
		if h.Logger != nil {
			h.Logger.Warn(msg, append([]any{"upstream", upstream}, kv...)...)
		}
	}
}

func (h *Handler) maxBody() int64 {
	if h.MaxBody <= 0 {
		return 8 << 20
	}
	return h.MaxBody
}

// readBody buffers the request for replay. Oversized bodies lose replay.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limit := h.maxBody()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return nil, false
	}
	if int64(len(body)) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds max_buffered_body")
		return nil, false
	}
	return body, true
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request) {
	var list []modelInfo
	for _, canonical := range h.Catalog.CanonicalModels() {
		available := 0
		for _, u := range h.Reg.Candidates(canonical) {
			if u.Available() {
				available++
			}
		}
		if available == 0 {
			continue
		}
		list = append(list, modelInfo{ID: canonical, Object: "model", Created: time.Now().Unix(), OwnedBy: "relayd"})
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"object":"list","data":[`)
	for i, m := range list {
		if i > 0 {
			io.WriteString(w, ",")
		}
		io.WriteString(w, `{"id":"`+m.ID+`","object":"model","created":`+strconv.FormatInt(m.Created, 10)+`,"owned_by":"relayd"}`)
	}
	io.WriteString(w, `]}`)
}
