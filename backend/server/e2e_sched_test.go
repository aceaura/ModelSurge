package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relayd/backend/account"
	"relayd/backend/config"
	"relayd/backend/server"
)

// newSchedGateway 起一个带账号调度器的网关。
func newSchedGateway(t *testing.T, ups []config.Upstream) (*httptest.Server, *account.Manager) {
	t.Helper()
	store, err := account.Open(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seeds := make([]account.SeedUpstream, len(ups))
	for i, u := range ups {
		seeds[i] = account.SeedUpstream{Name: u.Name, Protocol: u.Protocol, BaseURL: u.BaseURL, APIKey: u.APIKey, Models: u.Models}
	}
	m, err := account.NewManager(store, seeds, nil, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Upstreams: ups,
		Scheduler: &config.Scheduler{SameAccountRetries: 2},
	}
	gw := httptest.NewServer(server.New(cfg, m).Handler())
	t.Cleanup(gw.Close)
	return gw, m
}

// countingUpstream 带调用计数的 anthropic SSE mock 上游。
func countingUpstream(t *testing.T, calls *atomic.Int32, marker string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(upstreamStream("anthropic", marker)))
	}))
}

func chatReq(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader(anthropicChatBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// 429（带重置时间戳的真实样例）→ 账号冷却切号；
// 后续请求不再打到已冷却账号（粘性保住缓存命中）。
func TestSchedRateLimitSwitch(t *testing.T) {
	var calls1, calls2 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls1.Add(1)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"You have exceeded the 5-hour usage quota. It will reset at 2099-01-01 00:00:00 +0800 CST."}}`))
	}))
	defer up1.Close()
	up2 := countingUpstream(t, &calls2, "AFTER_SWITCH")
	defer up2.Close()

	gw, m := newSchedGateway(t, []config.Upstream{
		{Name: "bad", Protocol: "anthropic", BaseURL: up1.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
		{Name: "good", Protocol: "anthropic", BaseURL: up2.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	})

	status, body := chatReq(t, gw.URL)
	if status != 200 || !strings.Contains(body, "AFTER_SWITCH") {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if calls1.Load() != 1 {
		t.Errorf("429 account called %d times, want 1 (no in-place retry for rate limit)", calls1.Load())
	}
	// 账号冷却状态入库，且时间戳取自重置时刻（2099 年，即解析生效）
	var bad account.Account
	for _, a := range m.Status() {
		if a.Name == "bad" {
			bad = a
		}
	}
	if bad.LimitKind != "5h" || bad.CooldownUntil.Year() != 2099 {
		t.Errorf("cooldown state = kind %q until %v", bad.LimitKind, bad.CooldownUntil)
	}
	// 粘性：第二次请求不再打已冷却账号
	if _, body := chatReq(t, gw.URL); !strings.Contains(body, "AFTER_SWITCH") {
		t.Errorf("second request body = %s", body)
	}
	if calls1.Load() != 1 {
		t.Errorf("cooling account called again (%d times)", calls1.Load())
	}
	if calls2.Load() != 2 {
		t.Errorf("healthy account called %d times, want 2", calls2.Load())
	}
}

// 瞬时错误（5xx）→ 原地重试同账号，不切号（保缓存命中）。
func TestSchedTransientRetryInPlace(t *testing.T) {
	var calls1, calls2 atomic.Int32
	var attempt atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls1.Add(1)
		if attempt.Add(1) <= 2 { // 前两次 500
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		// 第三次恢复
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(upstreamStream("anthropic", "RECOVERED")))
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls2.Add(1)
		w.WriteHeader(500)
	}))
	defer up2.Close()

	gw, _ := newSchedGateway(t, []config.Upstream{
		{Name: "flaky", Protocol: "anthropic", BaseURL: up1.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
		{Name: "other", Protocol: "anthropic", BaseURL: up2.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	})

	status, body := chatReq(t, gw.URL)
	if status != 200 || !strings.Contains(body, "RECOVERED") {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if got := calls1.Load(); got != 3 {
		t.Errorf("flaky account called %d times, want 3 (2 failures + 1 success, in place)", got)
	}
	if got := calls2.Load(); got != 0 {
		t.Errorf("second account called %d times, want 0 (no unnecessary switch)", got)
	}
}

// 原地重试耗尽后才切号；401 直接禁用并切号。
func TestSchedAuthFailureDisables(t *testing.T) {
	var calls1, calls2 atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls1.Add(1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"The API key format is incorrect."}}`))
	}))
	defer up1.Close()
	up2 := countingUpstream(t, &calls2, "OK")
	defer up2.Close()

	gw, m := newSchedGateway(t, []config.Upstream{
		{Name: "dead-key", Protocol: "anthropic", BaseURL: up1.URL, APIKey: "bad", Models: map[string]string{"m": "m"}},
		{Name: "good", Protocol: "anthropic", BaseURL: up2.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	})

	status, body := chatReq(t, gw.URL)
	if status != 200 || !strings.Contains(body, "OK") {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if calls1.Load() != 1 {
		t.Errorf("401 account called %d times, want 1 (auth failure: no retry)", calls1.Load())
	}
	for _, a := range m.Status() {
		if a.Name == "dead-key" && !a.Disabled {
			t.Error("401 account should be disabled")
		}
	}
	// 禁用后不再被选中
	if _, body := chatReq(t, gw.URL); !strings.Contains(body, "OK") {
		t.Errorf("second request body = %s", body)
	}
	if calls1.Load() != 1 {
		t.Errorf("disabled account called again (%d)", calls1.Load())
	}
}

// 请求成功 → 真实 usage 记账入库（message_start 的 input 与 delta 的 output 合并）。
func TestSchedUsageRecorded(t *testing.T) {
	up, _ := mockUpstream(t, "anthropic", "OK")
	gw, m := newSchedGateway(t, []config.Upstream{
		{Name: "acc1", Protocol: "anthropic", BaseURL: up.URL, APIKey: "k", Models: map[string]string{"m": "m"}},
	})

	status, body := chatReq(t, gw.URL)
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	// mock 流：message_start input=10 output=1 + message_delta output=5
	u, err := m.WindowUsage("acc1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Errorf("recorded usage = %+v, want input=10 output=5", u)
	}
}
