package server_test

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"relayd/backend/config"
)

// lockedWriter log 输出的并发安全捕获（请求处理中 relay 层也会写日志）。
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestAccessLog 开启访问日志后 200/401 均被记录；
// 流式响应经 statusRecorder 包装后仍逐块刷出（Flusher 透传不被破坏）。
func TestAccessLog(t *testing.T) {
	lw := &lockedWriter{}
	log.SetOutput(lw)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	up, _ := mockUpstream(t, "anthropic", "STREAM_OK")
	gw := newGateway(t, &config.Config{
		APIKey:           "k",
		AccessLogEnabled: true,
		Upstreams: []config.Upstream{
			{Name: "cl", Protocol: "anthropic", BaseURL: up.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
		},
	})

	resp, err := http.Get(gw.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest("POST", gw.URL+"/v1/messages", strings.NewReader(anthropicChatBody()))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest("POST", gw.URL+"/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "k")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "STREAM_OK") {
		t.Fatalf("status = %d, body = %s, want streamed marker", resp.StatusCode, body)
	}

	logs := lw.String()
	for _, want := range []string{
		"200 GET /health",
		"401 POST /v1/messages",
		"200 POST /v1/messages",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("access log missing %q:\n%s", want, logs)
		}
	}
}

// TestAccessLogDisabled 关闭后不产生访问日志。
func TestAccessLogDisabled(t *testing.T) {
	lw := &lockedWriter{}
	log.SetOutput(lw)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	gw := newGateway(t, &config.Config{
		AccessLogEnabled: false,
		Upstreams: []config.Upstream{
			{Name: "cl", Protocol: "anthropic", BaseURL: "http://127.0.0.1:1", APIKey: "k", Models: map[string]string{"m": "m"}},
		},
	})
	resp, err := http.Get(gw.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if s := lw.String(); strings.Contains(s, "GET /health") {
		t.Errorf("access log should be silent when disabled:\n%s", s)
	}
}
