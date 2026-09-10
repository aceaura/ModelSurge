package relayd_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"relayd/backend/catalog"
	"relayd/backend/egress"
	"relayd/backend/ingress"
	"relayd/strategy"
)

// Cross-protocol conversion against standard payloads (same shapes relaymock
// serves): both directions, stream and non-stream, plus native model rewrite.

func newCrossStack(t *testing.T, upstreamProtocol, nativeModel string, aliases []string, upstream http.HandlerFunc) *httptest.Server {
	t.Helper()
	upSrv := httptest.NewServer(upstream)
	t.Cleanup(upSrv.Close)

	aliasLine := ""
	if len(aliases) > 0 {
		aliasLine = "    aliases: [" + strings.Join(aliases, ", ") + "]"
	}
	cfgYAML := fmt.Sprintf(`
listen: "127.0.0.1:0"
admin_listen: "127.0.0.1:1"
auth_tokens: ["tok"]
models:
  claude-sonnet-4:
    protocol_hint: claude
%s
providers:
  up:
    protocol: %s
credentials:
  - provider: up
    name: up-01
    base_url: %s
    api_key: sk-up
    models: [%s]
`, aliasLine, upstreamProtocol, upSrv.URL, nativeModel)

	cfgPath := writeTempConfig(t, cfgYAML)
	cfg, err := catalog.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.NewCatalog(cfg.Models)
	ups, err := cfg.BuildUpstreams(cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := strategy.NewRegistry()
	reg.Replace(ups)

	h := &ingress.Handler{
		Reg: reg, Catalog: cat, Fwd: egress.NewForwarder(),
		Tokens: map[string]bool{"tok": true}, MaxBody: 1 << 20,
	}
	biz := httptest.NewServer(h.Mux())
	t.Cleanup(biz.Close)
	return biz
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	p := t.TempDir() + "/relayd.yaml"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func post(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-api-key", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ---- Claude client -> OpenAI upstream ----

func openAIUpstream(t *testing.T, nativeModel string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(body, "model").String(); got != nativeModel {
			t.Errorf("upstream must receive native model %q, got %q", nativeModel, got)
		}
		if !gjson.GetBytes(body, "messages").Exists() {
			t.Errorf("openai request must carry messages: %s", body)
		}
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\""+nativeModel+"\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\""+nativeModel+"\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello cross\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\""+nativeModel+"\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+nativeModel+`",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello cross"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}
}

func TestCrossProtocol_ClaudeClientOpenAIUpstream_NonStream(t *testing.T) {
	biz := newCrossStack(t, "openai", "claude-3-5-sonnet-20241022", []string{"claude-3-5-sonnet-20241022"}, openAIUpstream(t, "claude-3-5-sonnet-20241022"))
	code, body := post(t, biz.URL+"/v1/messages", "tok",
		`{"model":"claude-sonnet-4","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if gjson.Get(body, "type").String() != "message" {
		t.Fatalf("must be a claude message: %s", body)
	}
	if got := gjson.Get(body, "content.0.text").String(); got != "hello cross" {
		t.Fatalf("content mismatch: %q in %s", got, body)
	}
	if got := gjson.Get(body, "stop_reason").String(); got != "end_turn" {
		t.Fatalf("stop_reason must map stop->end_turn, got %q", got)
	}
	if got := gjson.Get(body, "model").String(); got != "claude-sonnet-4" {
		t.Fatalf("response model must be canonical, got %q", got)
	}
	if gjson.Get(body, "usage.output_tokens").Int() != 3 {
		t.Fatalf("usage must map, body: %s", body)
	}
}

func TestCrossProtocol_ClaudeClientOpenAIUpstream_Stream(t *testing.T) {
	biz := newCrossStack(t, "openai", "claude-3-5-sonnet-20241022", []string{"claude-3-5-sonnet-20241022"}, openAIUpstream(t, "claude-3-5-sonnet-20241022"))
	code, body := post(t, biz.URL+"/v1/messages", "tok",
		`{"model":"claude-sonnet-4","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, want := range []string{`"type":"message_start"`, `"type":"content_block_delta"`, "hello cross", `"type":"message_stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("openai chunks leaked into claude stream:\n%s", body)
	}
}

// ---- OpenAI client -> Claude upstream ----

func claudeUpstream(t *testing.T, nativeModel string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(body, "model").String(); got != nativeModel {
			t.Errorf("upstream must receive native model %q, got %q", nativeModel, got)
		}
		if !gjson.GetBytes(body, "max_tokens").Exists() {
			t.Errorf("claude request must carry max_tokens (default injected): %s", body)
		}
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\""+nativeModel+"\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n")
			fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello back\"}}\n\n")
			fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg-1","type":"message","role":"assistant","model":"`+nativeModel+`",`+
			`"content":[{"type":"text","text":"hello back"}],"stop_reason":"end_turn","stop_sequence":null,`+
			`"usage":{"input_tokens":5,"output_tokens":3}}`)
	}
}

func TestCrossProtocol_OpenAIClientClaudeUpstream_NonStream(t *testing.T) {
	biz := newCrossStack(t, "claude", "claude-sonnet-4", nil, claudeUpstream(t, "claude-sonnet-4"))
	code, body := post(t, biz.URL+"/v1/chat/completions", "tok",
		`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if got := gjson.Get(body, "choices.0.message.content").String(); got != "hello back" {
		t.Fatalf("content mismatch: %q in %s", got, body)
	}
	if got := gjson.Get(body, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason must map end_turn->stop, got %q", got)
	}
	if gjson.Get(body, "usage.completion_tokens").Int() != 3 {
		t.Fatalf("usage must map, body: %s", body)
	}
}

func TestCrossProtocol_OpenAIClientClaudeUpstream_Stream(t *testing.T) {
	biz := newCrossStack(t, "claude", "claude-sonnet-4", nil, claudeUpstream(t, "claude-sonnet-4"))
	code, body := post(t, biz.URL+"/v1/chat/completions", "tok",
		`{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, want := range []string{"chat.completion.chunk", "hello back", `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "content_block_delta") {
		t.Fatalf("claude events leaked into openai stream:\n%s", body)
	}
}
