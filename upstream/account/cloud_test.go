package account

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withCloud 安装云中转配置并测试毕恢复。
func withCloud(t *testing.T, cfg CloudConfig) {
	t.Helper()
	old := cloudCfg
	SetCloudConfig(cfg)
	t.Cleanup(func() { cloudCfg = old })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestCloudTransport_Forward：kiro host 请求被重写到转发地址，
// 携带 X-Kiro-Target-Url / X-Cloud-Key / X-Kiro-Authorization /
// x-amz-content-sha256，Authorization 头移除。
func TestCloudTransport_Forward(t *testing.T) {
	var gotTarget, gotKey, gotAuth, gotSHA, gotBody string
	var sawAuthHeader bool
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTarget = r.Header.Get("X-Kiro-Target-Url")
		gotKey = r.Header.Get("X-Cloud-Key")
		gotAuth = r.Header.Get("X-Kiro-Authorization")
		gotSHA = r.Header.Get("x-amz-content-sha256")
		sawAuthHeader = r.Header.Get("Authorization") != ""
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("from-cloud"))
	}))
	t.Cleanup(cloud.Close)

	withCloud(t, CloudConfig{ForwardURL: cloud.URL, APIKey: "ck-123"})
	client := &http.Client{Transport: WrapCloudTransport(nil)}
	req, _ := http.NewRequest(http.MethodPost, "https://q.us-east-1.amazonaws.com/mcp",
		strings.NewReader(`{"query":"go"}`))
	req.Header.Set("Authorization", "Bearer at-1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(respBody) != "from-cloud" {
		t.Fatalf("resp = %s", respBody)
	}
	if gotTarget != "https://q.us-east-1.amazonaws.com/mcp" {
		t.Errorf("X-Kiro-Target-Url = %q", gotTarget)
	}
	if gotKey != "ck-123" {
		t.Errorf("X-Cloud-Key = %q", gotKey)
	}
	if gotAuth != "Bearer at-1" {
		t.Errorf("X-Kiro-Authorization = %q", gotAuth)
	}
	if sawAuthHeader {
		t.Errorf("Authorization header must be moved, not duplicated")
	}
	if len(gotSHA) != 64 { // sha256 hex
		t.Errorf("x-amz-content-sha256 = %q, want 64 hex chars", gotSHA)
	}
	if gotBody != `{"query":"go"}` {
		t.Errorf("forwarded body = %q", gotBody)
	}
}

// TestCloudTransport_PassThrough：非 Kiro host 直通，无云转发头。
func TestCloudTransport_PassThrough(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Cloud-Key") != "" {
			t.Errorf("cloud headers leaked to non-kiro host")
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(direct.Close)

	withCloud(t, CloudConfig{ForwardURL: "http://127.0.0.1:1/forward", APIKey: "k"})
	client := &http.Client{Transport: WrapCloudTransport(nil)}
	resp, err := client.Get(direct.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// TestCloudTransport_Fallback：转发端网络失败 → 回退直连：
// 原始 URL、载荷与鉴权头原样重放。
func TestCloudTransport_Fallback(t *testing.T) {
	const origURL = "https://q.us-east-1.amazonaws.com/mcp"
	var directURL, directAuth, directBody string
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "forward.example.com") { // 云转发尝试：模拟网络失败
			return nil, errNetDown
		}
		// 回退直连：URL 还原为原始 kiro 地址
		directURL = r.URL.String()
		directAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		directBody = string(b)
		return &http.Response{
			StatusCode: 200, Body: io.NopCloser(strings.NewReader("direct-ok")),
			Header: http.Header{}, Request: r,
		}, nil
	})
	// 转发地址 host 非 kiro 形态（走 next 时第一个分支报错模拟网络失败）
	withCloud(t, CloudConfig{ForwardURL: "https://forward.example.com/forward", APIKey: "k"})
	client := &http.Client{Transport: &cloudTransport{next: next}}
	req, _ := http.NewRequest(http.MethodPost, origURL, strings.NewReader(`{"q":1}`))
	req.Header.Set("Authorization", "Bearer at-2")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(respBody) != "direct-ok" {
		t.Fatalf("fallback resp = %s", respBody)
	}
	if directURL != origURL {
		t.Errorf("direct URL = %q, want original %q", directURL, origURL)
	}
	if directAuth != "Bearer at-2" {
		t.Errorf("direct Authorization = %q", directAuth)
	}
	if directBody != `{"q":1}` {
		t.Errorf("direct body = %q", directBody)
	}
}

type netErr struct{}

func (netErr) Error() string { return "net down" }

var errNetDown error = netErr{}
