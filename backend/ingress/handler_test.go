package ingress

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relayd/backend/catalog"
	"relayd/backend/egress"
	"relayd/strategy"
)

type mockUpstream struct {
	server  *httptest.Server
	hits    atomic.Int32
	handler func(w http.ResponseWriter, r *http.Request)
}

func (m *mockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.hits.Add(1)
	m.handler(w, r)
}

func newMock(handler func(w http.ResponseWriter, r *http.Request)) *mockUpstream {
	m := &mockUpstream{handler: handler}
	m.server = httptest.NewServer(m)
	return m
}

func okJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
}

func buildHandler(t *testing.T, mocks map[string]*mockUpstream, protocols map[string]string) (*Handler, map[string]*strategy.Upstream) {
	t.Helper()
	cfg := &catalog.Config{
		Models: map[string]catalog.ModelSpec{
			"gpt-4o": {ProtocolHint: "openai"},
		},
	}
	cat := catalog.NewCatalog(cfg.Models)

	reg := strategy.NewRegistry()
	ups := map[string]*strategy.Upstream{}
	for name, m := range mocks {
		c := strategy.NewCircuit(strategy.CircuitConfig{
			FailThreshold:  3,
			OpenBase:       50 * time.Millisecond,
			OpenMax:        time.Minute,
			HalfOpenProbes: 1,
			SuccessToClose: 1,
		}, false, time.Now, nil)
		c.SetAutoBan(true)
		proto := protocols[name]
		if proto == "" {
			proto = "openai"
		}
		u := &strategy.Upstream{
			Name:        name,
			Protocol:    proto,
			BaseURL:     m.server.URL,
			APIKey:      "sk-test",
			Models:      []string{"gpt-4o"},
			NativeModel: map[string]string{"gpt-4o": "gpt-4o"},
			Priority:    0,
			Weight:      1,
			Circuit:     c,
		}
		ups[name] = u
	}
	var all []*strategy.Upstream
	for _, u := range ups {
		all = append(all, u)
	}
	reg.Replace(all)

	h := &Handler{
		Reg:     reg,
		Catalog: cat,
		Fwd:     egress.NewForwarder(),
		Tokens:  map[string]bool{"tok": true},
		MaxBody: 1 << 20,
	}
	return h, ups
}

func doChat(t *testing.T, h *Handler, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, req)
	resp := w.Result()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRelay_FailoverSkipsFailedUpstream(t *testing.T) {
	bad := newMock(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	})
	good := newMock(okJSON)
	defer bad.server.Close()
	defer good.server.Close()

	h, ups := buildHandler(t, map[string]*mockUpstream{"bad": bad, "good": good}, nil)
	// force bad to be picked first deterministically
	ups["bad"].Weight = 100

	code, body := doChat(t, h, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("want 200 after failover, got %d: %s", code, body)
	}
	if bad.hits.Load() != 1 || good.hits.Load() != 1 {
		t.Fatalf("want bad=1 good=1, got bad=%d good=%d", bad.hits.Load(), good.hits.Load())
	}
}

func TestRelay_429CooldownRedirectsTraffic(t *testing.T) {
	limited := newMock(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	good := newMock(okJSON)
	defer limited.server.Close()
	defer good.server.Close()

	h, ups := buildHandler(t, map[string]*mockUpstream{"limited": limited, "good": good}, nil)
	ups["limited"].Weight = 100

	code, _ := doChat(t, h, `{"model":"gpt-4o","messages":[]}`)
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	if limited.hits.Load() != 1 {
		t.Fatalf("want limited hit once, got %d", limited.hits.Load())
	}
	// limited is now cooling down; subsequent requests go straight to good
	for i := 0; i < 3; i++ {
		doChat(t, h, `{"model":"gpt-4o","messages":[]}`)
	}
	if limited.hits.Load() != 1 {
		t.Fatalf("cooling upstream must not be retried, hits=%d", limited.hits.Load())
	}
}

func TestRelay_400PassthroughNoFailover(t *testing.T) {
	a := newMock(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad request"}}`)
	})
	b := newMock(okJSON)
	defer a.server.Close()
	defer b.server.Close()

	h, ups := buildHandler(t, map[string]*mockUpstream{"a": a, "b": b}, nil)
	ups["a"].Weight = 100

	code, body := doChat(t, h, `{"model":"gpt-4o","messages":[]}`)
	if code != 400 || !strings.Contains(body, "bad request") {
		t.Fatalf("want 400 passthrough, got %d: %s", code, body)
	}
	if b.hits.Load() != 0 {
		t.Fatal("400 must not trigger failover")
	}
	if ups["a"].Circuit.Snapshot().ConsecFail != 0 {
		t.Fatal("client errors must not count against upstream")
	}
}

func TestRelay_UnknownModelRejected(t *testing.T) {
	m := newMock(okJSON)
	defer m.server.Close()
	h, _ := buildHandler(t, map[string]*mockUpstream{"a": m}, nil)

	code, _ := doChat(t, h, `{"model":"nonexistent","messages":[]}`)
	if code != 400 {
		t.Fatalf("want 400, got %d", code)
	}
	if m.hits.Load() != 0 {
		t.Fatal("unknown model must not reach upstream")
	}
}

func TestRelay_AuthRequired(t *testing.T) {
	m := newMock(okJSON)
	defer m.server.Close()
	h, _ := buildHandler(t, map[string]*mockUpstream{"a": m}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestRelay_AllExhausted503(t *testing.T) {
	m := newMock(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer m.server.Close()
	h, ups := buildHandler(t, map[string]*mockUpstream{"a": m}, nil)

	code, _ := doChat(t, h, `{"model":"gpt-4o","messages":[]}`)
	if code != 503 {
		t.Fatalf("want 503, got %d", code)
	}
	_ = ups
}

func TestRelay_SSEPassthrough(t *testing.T) {
	m := newMock(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	defer m.server.Close()
	h, _ := buildHandler(t, map[string]*mockUpstream{"a": m}, nil)

	code, body := doChat(t, h, `{"model":"gpt-4o","stream":true,"messages":[]}`)
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	if !strings.Contains(body, "he") || !strings.Contains(body, "llo") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("SSE frames must pass through, got %q", body)
	}
}

func TestRelay_ClaudeIngressToOpenAIUpstream(t *testing.T) {
	var gotAuth, gotBody string
	m := newMock(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		okJSON(w, r)
	})
	defer m.server.Close()

	h, _ := buildHandler(t, map[string]*mockUpstream{"a": m}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("egress must use Bearer auth, got %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"gpt-4o"`) {
		t.Fatalf("converted request body missing model: %s", gotBody)
	}
}

func TestRelay_ModelsEndpointOnlyAvailable(t *testing.T) {
	m := newMock(okJSON)
	defer m.server.Close()
	h, ups := buildHandler(t, map[string]*mockUpstream{"a": m}, nil)

	code, _ := doChat(t, h, `{"model":"gpt-4o","messages":[]}`)
	if code != 200 {
		t.Fatal(code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "gpt-4o") {
		t.Fatalf("models must list available canonical: %s", w.Body.String())
	}

	ups["a"].Circuit.ManualDisable("test")
	w2 := httptest.NewRecorder()
	h.Mux().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	// note: this second request lacks auth; ensure list still reflects state
	// via authed request
	req3 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer tok")
	w3 := httptest.NewRecorder()
	h.Mux().ServeHTTP(w3, req3)
	if strings.Contains(w3.Body.String(), "gpt-4o") {
		t.Fatalf("model with zero available upstreams must be hidden: %s", w3.Body.String())
	}
}
