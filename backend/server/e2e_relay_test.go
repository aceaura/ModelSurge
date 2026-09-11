package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relayd/backend/config"
	"relayd/backend/server"
)

func newGateway(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	gw := httptest.NewServer(server.New(cfg).Handler())
	t.Cleanup(gw.Close)
	return gw
}

func anthropicChatBody() string {
	return `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
}

// TestRetryOnUpstreamFailure 首个上游 5xx 时换下一个上游重发（未写字节原则）。
func TestRetryOnUpstreamFailure(t *testing.T) {
	var calls1 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls1.Add(1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer up1.Close()
	up2, rec2 := mockUpstream(t, "anthropic", "FROM_SECOND")

	gw := newGateway(t, &config.Config{Upstreams: []config.Upstream{
		{Name: "bad", Protocol: "anthropic", BaseURL: up1.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
		{Name: "good", Protocol: "anthropic", BaseURL: up2.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	}})

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(anthropicChatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "FROM_SECOND") {
		t.Errorf("response missing marker from second upstream:\n%s", body)
	}
	if calls1.Load() != 1 {
		t.Errorf("first upstream called %d times, want 1", calls1.Load())
	}
	if rec2.body == "" {
		t.Error("second upstream was not called")
	}
}

// TestRetryOnFirstTokenTimeout 上游建连成功但迟迟不出首事件时，超时换上游。
func TestRetryOnFirstTokenTimeout(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // 挂住，直到网关取消
	}))
	defer hang.Close()
	up2, _ := mockUpstream(t, "anthropic", "AFTER_TIMEOUT")

	gw := newGateway(t, &config.Config{
		FirstTokenTimeoutDur: 50 * time.Millisecond,
		Upstreams: []config.Upstream{
			{Name: "hang", Protocol: "anthropic", BaseURL: hang.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
			{Name: "good", Protocol: "anthropic", BaseURL: up2.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
		},
	})

	start := time.Now()
	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(anthropicChatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "AFTER_TIMEOUT") {
		t.Errorf("response missing marker:\n%s", body)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v, first-token timeout did not fire promptly", elapsed)
	}
}

// TestNoRetryAfterBytesWritten 流式响应中途断流不换上游：错误在流内渲染。
func TestNoRetryAfterBytesWritten(t *testing.T) {
	var calls2 atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// 只发半截流就关闭
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"x\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
	}))
	defer broken.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls2.Add(1)
		w.WriteHeader(200)
	}))
	defer up2.Close()

	gw := newGateway(t, &config.Config{Upstreams: []config.Upstream{
		{Name: "broken", Protocol: "anthropic", BaseURL: broken.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
		{Name: "other", Protocol: "anthropic", BaseURL: up2.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	}})

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(anthropicChatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if calls2.Load() != 0 {
		t.Errorf("second upstream called %d times after bytes were written, want 0", calls2.Load())
	}
}

// TestCountTokensNativeForward 有 anthropic 上游时 count_tokens 原生转发（模型名映射）。
func TestCountTokensNativeForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"model":"native-m"`) {
				t.Errorf("count_tokens upstream body missing native model: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":42}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer up.Close()

	gw := newGateway(t, &config.Config{Upstreams: []config.Upstream{
		{Name: "cl", Protocol: "anthropic", BaseURL: up.URL, APIKey: "k", Models: map[string]string{"m": "native-m"}},
	}})

	resp, err := http.Post(gw.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(anthropicChatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"input_tokens":42`) {
		t.Errorf("status = %d, body = %s, want passthrough of upstream count", resp.StatusCode, body)
	}
}

// TestCountTokensLocalEstimate 无 anthropic 上游时本地估算兜底。
func TestCountTokensLocalEstimate(t *testing.T) {
	up, _ := mockUpstream(t, "openai-chat", "X")
	gw := newGateway(t, &config.Config{Upstreams: []config.Upstream{
		{Name: "st", Protocol: "openai-chat", BaseURL: up.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	}})

	resp, err := http.Post(gw.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(anthropicChatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"input_tokens":`) || strings.Contains(string(body), `"input_tokens":0`) {
		t.Errorf("estimated count missing or zero: %s", body)
	}
}

// TestHostedToolMapping Anthropic web_search 声明映射到 Gemini googleSearch。
func TestHostedToolMapping(t *testing.T) {
	up, rec := mockUpstream(t, "gemini", "OK")
	gw := newGateway(t, &config.Config{Upstreams: []config.Upstream{
		{Name: "gm", Protocol: "gemini", BaseURL: up.URL, APIKey: "k", Models: map[string]string{"m": "native-m"}},
	}})

	reqBody := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"get_weather","input_schema":{"type":"object"}}]}`
	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if !strings.Contains(rec.body, `"googleSearch"`) {
		t.Errorf("gemini upstream body missing googleSearch tool:\n%s", rec.body)
	}
	if !strings.Contains(rec.body, `"get_weather"`) {
		t.Errorf("gemini upstream body missing function tool:\n%s", rec.body)
	}
}

// TestHostedToolRoundTrip Anthropic web_search 同协议往返保持带版本 type。
func TestHostedToolRoundTrip(t *testing.T) {
	up, rec := mockUpstream(t, "anthropic", "OK")
	gw := newGateway(t, &config.Config{Upstreams: []config.Upstream{
		{Name: "cl", Protocol: "anthropic", BaseURL: up.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	}})

	reqBody := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if !strings.Contains(rec.body, `"web_search_20250305"`) {
		t.Errorf("anthropic upstream body missing versioned web_search type:\n%s", rec.body)
	}
}

// TestDiagnosticsHeader 有损转换经 X-Relayd-Notes 响应头暴露。
func TestDiagnosticsHeader(t *testing.T) {
	up, _ := mockUpstream(t, "openai-chat", "OK")
	gw := newGateway(t, &config.Config{Upstreams: []config.Upstream{
		{Name: "st", Protocol: "openai-chat", BaseURL: up.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	}})

	// 带签名 thinking 块 + hosted 工具：openai-chat 两者都不支持
	reqBody := `{"model":"m","max_tokens":100,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig123"},{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":"go on"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	notes := resp.Header.Get("X-Relayd-Notes")
	if !strings.Contains(notes, "thinking signature") {
		t.Errorf("notes missing thinking signature drop: %q", notes)
	}
	if !strings.Contains(notes, "dropped hosted tool") {
		t.Errorf("notes missing hosted tool drop: %q", notes)
	}
}

// TestUsageEstimation 开启 estimate_usage 后上游不报 usage 时本地估算。
func TestUsageEstimation(t *testing.T) {
	// mock 上游不发 usage chunk
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hello world"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`))
	}))
	defer up.Close()

	gw := newGateway(t, &config.Config{
		EstimateUsage: true,
		Upstreams: []config.Upstream{
			{Name: "st", Protocol: "openai-chat", BaseURL: up.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
		},
	})

	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	s := string(body)
	if !strings.Contains(s, `"prompt_tokens":`) || !strings.Contains(s, `"completion_tokens":`) {
		t.Errorf("estimated usage missing: %s", s)
	}
	if strings.Contains(s, `"completion_tokens":0`) {
		t.Errorf("completion_tokens estimated as zero: %s", s)
	}
}
