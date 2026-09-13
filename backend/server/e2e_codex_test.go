package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"relayd/backend/config"
)

// codex 账号端到端：客户端 openai-chat（reasoning_effort=xhigh）→ IR →
// codex 上游（/responses 路径、配套身份头、store=false、reasoning.effort 透传）→
// 上游 Responses SSE 聚合回客户端 chat JSON。
func TestCodexUpstreamEndToEnd(t *testing.T) {
	type codexReq struct {
		path    string
		headers http.Header
		body    string
	}
	rec := make(chan codexReq, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case rec <- codexReq{r.URL.Path, r.Header.Clone(), string(body)}:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(upstreamStream("openai-responses", "SOL-OK")))
	}))
	defer up.Close()

	acc := apiKeyAcc("codex-sub-1", "codex", up.URL, "gpt-5.6-sol", "gpt-5.6-sol")
	acc.Headers = map[string]string{"chatgpt-account-id": "acc-123"}
	gw := newGateway(t, &config.Config{}, acc)

	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-sol","stream":false,"reasoning_effort":"xhigh","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	clientBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, clientBody)
	}
	if !strings.Contains(string(clientBody), "SOL-OK") {
		t.Fatalf("client response missing marker: %s", clientBody)
	}

	select {
	case got := <-rec:
		if got.path != "/responses" {
			t.Fatalf("upstream path = %q, want /responses", got.path)
		}
		h := got.headers
		if h.Get("originator") != "codex-tui" {
			t.Fatalf("originator = %q", h.Get("originator"))
		}
		if !strings.HasPrefix(h.Get("User-Agent"), "codex-tui/") {
			t.Fatalf("User-Agent = %q", h.Get("User-Agent"))
		}
		if h.Get("version") == "" || h.Get("OpenAI-Beta") != "responses=experimental" {
			t.Fatalf("version/OpenAI-Beta = %q / %q", h.Get("version"), h.Get("OpenAI-Beta"))
		}
		if h.Get("chatgpt-account-id") != "acc-123" {
			t.Fatalf("chatgpt-account-id = %q", h.Get("chatgpt-account-id"))
		}
		if h.Get("session_id") == "" {
			t.Fatalf("session_id empty")
		}
		b := got.body
		for _, want := range []string{
			`"model":"gpt-5.6-sol"`,
			`"instructions":`,
			`"store":false`,
			`"effort":"xhigh"`,
			`"max_output_tokens":1024`,
			`"stream":true`,
			`reasoning.encrypted_content`,
		} {
			if !strings.Contains(b, want) {
				t.Fatalf("upstream body missing %s: %s", want, b)
			}
		}
	default:
		t.Fatal("upstream never received request")
	}
}
