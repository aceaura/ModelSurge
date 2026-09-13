// e2e_probe_test.go 上游端点自动探测适配的端到端测试：裸域名建号收敛、
// 带 /v1 不双拼、PUT 沿用值跳过探测、/test 修正根地址。
// 探测 client 注入真实 http.DefaultClient，上游为 httptest mock。
package account_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"relayd/backend/account"
	"relayd/backend/config"
	"relayd/backend/server"
)

// newProbeEnv 探测 e2e 环境：管理面网关 + 独立 mock 上游。
// upstream 的 /v1/models 与 /api/v1/models 返回模型列表，其余 404；
// 入口侧 /v1/chat/completions 会命中 chatFn（mock 一个 chat 端点）。
func newProbeEnv(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	store, err := account.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m, err := account.NewManager(store, account.Cooldowns{}, account.ManagerDeps{})
	if err != nil {
		t.Fatal(err)
	}
	m.SetProbeRateForTest(0)
	cfg := &config.Config{
		Scheduler: &config.Scheduler{SameAccountRetries: 2},
		Admin:     &config.Admin{APIKey: adminKey},
	}
	srv := server.New(cfg, m) // 探测 client 保持 http.DefaultClient，直连 httptest 上游
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(gw.Close)
	return gw
}

// probeMockUpstream mock 上游：modelsPath 命中时返回 openai 模型列表。
func probeMockUpstream(t *testing.T, modelsPath string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == modelsPath {
			if hits != nil {
				hits.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"mock-model"}]}`))
			return
		}
		w.WriteHeader(404)
	}))
}

// probeReport 从 admin 响应 JSON 中取 probe 字段。
func probeReport(t *testing.T, body string) *account.ProbeReport {
	t.Helper()
	var raw struct {
		Probe *account.ProbeReport `json:"probe"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode response: %v\n%s", err, body)
	}
	return raw.Probe
}

// 裸域名建号：探测收敛到根地址（写入 base_url），响应带 resolved 报告与模型列表。
func TestE2EProbeBareDomainCreate(t *testing.T) {
	var hits atomic.Int64
	up := probeMockUpstream(t, "/v1/models", &hits)
	gw := newProbeEnv(t, up)

	// host:port 形态的裸地址（httptest.URL 去掉 scheme）
	bare := strings.TrimPrefix(up.URL, "http://")
	body := `{"name":"p1","type":"api-key","protocol":"openai-chat","base_url":"` + bare + `","api_key":"k"}`
	status, resp := adminDo(t, "POST", gw.URL+"/admin/accounts", body, adminKey)
	if status != 201 {
		t.Fatalf("create: status = %d, body = %s", status, resp)
	}
	pr := probeReport(t, resp)
	if pr == nil || !pr.OK || pr.Verdict != "resolved" {
		t.Fatalf("probe = %+v, want resolved", pr)
	}
	if !strings.Contains(resp, `"base_url":"`+up.URL+`"`) {
		t.Errorf("base_url not written back resolved root: %s", resp)
	}
	if len(pr.Models) != 1 || pr.Models[0] != "mock-model" {
		t.Errorf("models = %v", pr.Models)
	}
	if hits.Load() != 1 {
		t.Errorf("probe hits = %d, want 1", hits.Load())
	}
}

// 网关 /api/v1 形态：候选 2 胜出，根地址含 /api。
func TestE2EProbeAPIPrefixCreate(t *testing.T) {
	up := probeMockUpstream(t, "/api/v1/models", nil)
	gw := newProbeEnv(t, up)

	bare := strings.TrimPrefix(up.URL, "http://")
	body := `{"name":"p2","type":"api-key","protocol":"openai-chat","base_url":"` + bare + `","api_key":"k"}`
	status, resp := adminDo(t, "POST", gw.URL+"/admin/accounts", body, adminKey)
	if status != 201 {
		t.Fatalf("create: status = %d, body = %s", status, resp)
	}
	pr := probeReport(t, resp)
	if pr == nil || !pr.OK || pr.ResolvedBaseURL != up.URL+"/api" {
		t.Fatalf("probe = %+v, want resolved %s/api", pr, up.URL)
	}
	if !strings.Contains(resp, `"base_url":"`+up.URL+`/api"`) {
		t.Errorf("base_url not written back with /api: %s", resp)
	}
}

// 带 /v1 的 SDK 风格地址：规范化剥离后探测，无双拼。
func TestE2EProbeSDKStyleCreate(t *testing.T) {
	up := probeMockUpstream(t, "/v1/models", nil)
	gw := newProbeEnv(t, up)

	body := `{"name":"p3","type":"api-key","protocol":"openai-chat","base_url":"` + up.URL + `/v1","api_key":"k"}`
	status, resp := adminDo(t, "POST", gw.URL+"/admin/accounts", body, adminKey)
	if status != 201 {
		t.Fatalf("create: status = %d, body = %s", status, resp)
	}
	pr := probeReport(t, resp)
	if pr == nil || !pr.OK {
		t.Fatalf("probe = %+v, want resolved", pr)
	}
	if !strings.Contains(resp, `"base_url":"`+up.URL+`"`) {
		t.Errorf("base_url should be bare root (no double /v1): %s", resp)
	}
}

// 全候选 404：仍建号，报告 no_match，base_url 保持规范化输入。
func TestE2EProbeNoMatchStillCreates(t *testing.T) {
	up := probeMockUpstream(t, "/none", nil)
	gw := newProbeEnv(t, up)

	body := `{"name":"p4","type":"api-key","protocol":"openai-chat","base_url":"` + up.URL + `","api_key":"k"}`
	status, resp := adminDo(t, "POST", gw.URL+"/admin/accounts", body, adminKey)
	if status != 201 {
		t.Fatalf("create: status = %d, body = %s", status, resp)
	}
	pr := probeReport(t, resp)
	if pr == nil || pr.OK || pr.Verdict != "no_match" {
		t.Fatalf("probe = %+v, want no_match", pr)
	}
	for _, att := range pr.Attempts {
		if att.Verdict != "not_found" {
			t.Errorf("attempt %s verdict = %s, want not_found", att.URL, att.Verdict)
		}
	}
}

// PUT 沿用库内 base_url 且协议未变：跳过探测（响应无 probe 字段）。
func TestE2EProbeUpdateSkippedWhenUnchanged(t *testing.T) {
	up := probeMockUpstream(t, "/v1/models", nil)
	gw := newProbeEnv(t, up)

	body := `{"name":"p5","type":"api-key","protocol":"openai-chat","base_url":"` + up.URL + `","api_key":"k"}`
	if status, resp := adminDo(t, "POST", gw.URL+"/admin/accounts", body, adminKey); status != 201 {
		t.Fatalf("create: %d %s", status, resp)
	}
	// 仅改 enabled，不带 base_url
	status, resp := adminDo(t, "PUT", gw.URL+"/admin/accounts/p5", `{"enabled":false}`, adminKey)
	if status != 200 {
		t.Fatalf("update: %d %s", status, resp)
	}
	if strings.Contains(resp, `"probe"`) {
		t.Errorf("update with unchanged base_url should skip probe: %s", resp)
	}
}

// /test 全量探测：建号时上游未就绪（全 404），上游就绪后 /test 胜出并修正根地址。
func TestE2EProbeTestEndpointFixesRoot(t *testing.T) {
	var serve atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serve.Load() && r.URL.Path == "/api/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"mock-model"}]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer up.Close()
	gw := newProbeEnv(t, up)

	// 建号：上游未就绪 -> 全候选 404 -> no_match，仍建号
	body := `{"name":"p6","type":"api-key","protocol":"openai-chat","base_url":"` + up.URL + `/wrong","api_key":"k"}`
	status, resp := adminDo(t, "POST", gw.URL+"/admin/accounts", body, adminKey)
	if status != 201 {
		t.Fatalf("create: %d %s", status, resp)
	}
	if pr := probeReport(t, resp); pr == nil || pr.OK || pr.Verdict != "no_match" {
		t.Fatalf("probe on create = %+v, want no_match", pr)
	}

	// 上游就绪后 /test：裸 host 兜底候选探到 /api，根地址写回
	serve.Store(true)
	status, resp = adminDo(t, "POST", gw.URL+"/admin/accounts/p6/test", "", adminKey)
	if status != 200 {
		t.Fatalf("test: %d %s", status, resp)
	}
	pr := probeReport(t, resp)
	if pr == nil || !pr.OK || pr.ResolvedBaseURL != up.URL+"/api" {
		t.Fatalf("probe on test = %+v, want resolved %s/api", pr, up.URL)
	}
	status, resp = adminDo(t, "GET", gw.URL+"/admin/accounts/p6", "", adminKey)
	if status != 200 || !strings.Contains(resp, `"base_url":"`+up.URL+`/api"`) {
		t.Errorf("base_url not fixed by test: %d %s", status, resp)
	}
}

// kiro 账号不探测：create 响应无 probe 字段。
func TestE2EProbeKiroSkipped(t *testing.T) {
	gw, _, _ := newAdminEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	status, resp := adminDo(t, "POST", gw.URL+"/admin/accounts", kiroCreateBody, adminKey)
	if status != 201 {
		t.Fatalf("create: %d %s", status, resp)
	}
	if strings.Contains(resp, `"probe"`) {
		t.Errorf("kiro create should not carry probe: %s", resp)
	}
}
