// e2e_admin_test.go 管理面 REST API 端到端测试（任务组 8.5）。
// 经完整 HTTP 入口（X-Admin-Key 鉴权）覆盖：鉴权、CRUD 热生效、
// 脱敏、空凭据沿用、409/400 字段级错误、kiro 运维端点。
package account_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relayd/backend/account"
	"relayd/backend/config"
	"relayd/backend/server"
)

const adminKey = "admin-secret"

// newAdminEnv 起 mock Kiro 服务 + 开启管理面的网关（无预置账号）。
func newAdminEnv(t *testing.T, chat func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *account.Manager, *kiroMockState) {
	t.Helper()
	st := newKiroMock(t, chat)
	store, err := account.Open(filepath.Join(t.TempDir(), "admin.db"))
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
	gw := httptest.NewServer(newProbedServer(cfg, m).Handler())
	t.Cleanup(gw.Close)
	return gw, m, st
}

// deadTransport 探测替身：所有请求立即失败，避免既有用例的 api-key 建号
// 触发真实网络探测（DNS 解析拖慢测试）。
type deadTransport struct{}

func (deadTransport) RoundTrip(*http.Request) (resp *http.Response, err error) {
	return nil, errors.New("probe transport disabled in tests")
}

// newProbedServer 构造注入了 deadTransport 探测 client 的入口服务。
// 需要真实探测的用例（端点自适应 e2e）用 server.New 后自行 SetProbeClient。
func newProbedServer(cfg *config.Config, m *account.Manager) *server.Server {
	srv := server.New(cfg, m)
	srv.SetProbeClient(&http.Client{Transport: deadTransport{}})
	return srv
}

// adminDo 管理面请求（key 为空省略鉴权头）。
func adminDo(t *testing.T, method, url, body, key string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("X-Admin-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const kiroCreateBody = `{"name":"k1","type":"kiro","kiro":{"source":"refresh_token","refresh_token":"rt-abcdef123456"}}`

// 鉴权：错误/缺失 key 401；正确 key 200；未配置 admin 的网关 /admin 路径 404。
func TestE2EAdminAuth(t *testing.T) {
	gw, _, _ := newAdminEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	})
	for _, key := range []string{"", "wrong"} {
		if status, _ := adminDo(t, "GET", gw.URL+"/admin/accounts", "", key); status != 401 {
			t.Errorf("key %q: status = %d, want 401", key, status)
		}
	}
	if status, _ := adminDo(t, "GET", gw.URL+"/admin/accounts", "", adminKey); status != 200 {
		t.Errorf("correct key: status = %d, want 200", status)
	}
	// 客户端 api_key 鉴权不拦管理面：网关 api_key 为空时两套入口各自独立
	if status, _ := adminDo(t, "POST", gw.URL+"/admin/accounts", kiroCreateBody, "wrong"); status != 401 {
		t.Errorf("wrong key create: status = %d, want 401", status)
	}

	// 未配置 admin 的网关：/admin 不挂载
	plain, _, _ := newKiroEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	if status, _ := adminDo(t, "GET", plain.URL+"/admin/accounts", "", adminKey); status != 404 {
		t.Errorf("admin disabled gateway: status = %d, want 404", status)
	}
}

// kiro 账号生命周期：创建即调度 -> 脱敏 -> 409/400 -> 空凭据沿用 ->
// 熔断状态跨 PUT 保留 -> 删除移出调度。
func TestE2EAdminKiroAccountLifecycle(t *testing.T) {
	fail := true
	gw, m, st := newAdminEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroChatStream("ADMIN_LIFECYCLE"))
	})

	// 创建：201 + 脱敏（末 4 位）
	status, body := adminDo(t, "POST", gw.URL+"/admin/accounts", kiroCreateBody, adminKey)
	if status != 201 {
		t.Fatalf("create: status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"refresh_token":"****3456"`) {
		t.Errorf("create response not masked: %s", body)
	}
	// 原始凭据不泄漏
	if strings.Contains(body, "rt-abcdef123456") {
		t.Errorf("raw credential leaked: %s", body)
	}

	// 立即可调度
	fail = false
	status, body = postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 || !strings.Contains(body, "ADMIN_LIFECYCLE") {
		t.Fatalf("chat after create: status = %d, body = %s", status, body)
	}
	if got := st.chat.Load(); got != 1 {
		t.Errorf("kiro chat called %d times, want 1", got)
	}

	// 列表与详情：脱敏
	status, body = adminDo(t, "GET", gw.URL+"/admin/accounts", "", adminKey)
	if status != 200 || !strings.Contains(body, `"refresh_token":"****3456"`) {
		t.Errorf("list: status = %d, body = %s", status, body)
	}
	status, body = adminDo(t, "GET", gw.URL+"/admin/accounts/k1", "", adminKey)
	if status != 200 || !strings.Contains(body, `"refresh_token":"****3456"`) {
		t.Errorf("get: status = %d, body = %s", status, body)
	}

	// 409 重复创建
	if status, _ := adminDo(t, "POST", gw.URL+"/admin/accounts", kiroCreateBody, adminKey); status != 409 {
		t.Errorf("duplicate create: status = %d, want 409", status)
	}

	// 字段级 400
	for _, tc := range []struct{ body, wantField string }{
		{`{"name":"x","type":"kiro"}`, "kiro"},
		{`{"name":"x","type":"kiro","kiro":{"source":"refresh_token"}}`, "kiro.refresh_token"},
		{`{"name":"x","type":"api-key","protocol":"anthropic","base_url":"http://x"}`, "api_key"},
		{`{"name":"x","type":"api-key","protocol":"bogus","base_url":"http://x","api_key":"k"}`, "protocol"},
		{`{"name":"x","type":"bogus"}`, "type"},
	} {
		if status, body := adminDo(t, "POST", gw.URL+"/admin/accounts", tc.body, adminKey); status != 400 || !strings.Contains(body, tc.wantField) {
			t.Errorf("validate %s: status = %d, body = %s, want 400 mentioning %q", tc.body, status, body, tc.wantField)
		}
	}

	// 空凭据 PUT：refresh_token 留空沿用旧值
	status, body = adminDo(t, "PUT", gw.URL+"/admin/accounts/k1",
		`{"type":"kiro","kiro":{"source":"refresh_token"}}`, adminKey)
	if status != 200 {
		t.Fatalf("put with empty credential: status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"refresh_token":"****3456"`) {
		t.Errorf("put dropped old credential: %s", body)
	}
	// 凭据仍有效：chat 走通（token 需要用旧 refresh token 换新）
	if status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat); status != 200 || !strings.Contains(body, "ADMIN_LIFECYCLE") {
		t.Fatalf("chat after put: status = %d, body = %s", status, body)
	}

	// type 不可变
	if status, _ := adminDo(t, "PUT", gw.URL+"/admin/accounts/k1",
		`{"type":"api-key","protocol":"anthropic","base_url":"http://x","api_key":"k"}`, adminKey); status != 400 {
		t.Errorf("type change: status = %d, want 400", status)
	}

	// 熔断状态跨 PUT 保留：制造 500 熔断 -> PUT -> Failures/Cooldown 不丢
	fail = true
	// 单账号重试耗尽后熔断：客户端看到透传的上游错误（非 503——
	// 503 仅在从未调度到账号时出现）
	if status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat); status != 500 || !strings.Contains(body, "boom") {
		t.Fatalf("breaker request: status = %d, body = %s, want 500 with upstream error", status, body)
	}
	a := kiroAccountByName(m, "k1")
	if a.Failures != 1 || !a.CooldownUntil.After(time.Now()) {
		t.Fatalf("breaker state: failures = %d, cooldown = %v", a.Failures, a.CooldownUntil)
	}
	if status, _ := adminDo(t, "PUT", gw.URL+"/admin/accounts/k1",
		`{"type":"kiro","kiro":{"source":"refresh_token"}}`, adminKey); status != 200 {
		t.Fatalf("put during cooldown: status = %d", status)
	}
	a = kiroAccountByName(m, "k1")
	if a.Failures != 1 || !a.CooldownUntil.After(time.Now()) {
		t.Errorf("PUT reset runtime state: failures = %d, cooldown = %v", a.Failures, a.CooldownUntil)
	}

	// 删除：移出调度 -> 503（无可用账号）-> GET 404
	if status, _ := adminDo(t, "DELETE", gw.URL+"/admin/accounts/k1", "", adminKey); status != 204 {
		t.Fatalf("delete: status = %d", status)
	}
	if status, _ := adminDo(t, "GET", gw.URL+"/admin/accounts/k1", "", adminKey); status != 404 {
		t.Errorf("get after delete: status = %d, want 404", status)
	}
	chatCalls := st.chat.Load()
	if status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat); status != 503 {
		t.Fatalf("chat after delete: status = %d, body = %s", status, body)
	}
	if got := st.chat.Load(); got != chatCalls {
		t.Errorf("deleted account still serving: %d -> %d calls", chatCalls, got)
	}
}

// api-key 账号：创建 -> 调度 -> 空凭据 PUT 沿用。
func TestE2EAdminAPIKeyAccount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE("FROM_ADMIN_APIKEY")))
	}))
	t.Cleanup(up.Close)

	gw, m, _ := newAdminEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	status, body := adminDo(t, "POST", gw.URL+"/admin/accounts", fmt.Sprintf(
		`{"name":"a1","type":"api-key","protocol":"anthropic","base_url":%q,"api_key":"sk-1234567890abcd","models":{"claude-sonnet-4-5":"native-model"}}`, up.URL), adminKey)
	if status != 201 {
		t.Fatalf("create api-key: status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, `"api_key":"****abcd"`) {
		t.Errorf("api_key not masked: %s", body)
	}

	status, body = postChat(t, gw.URL, "/v1/messages", anthropicChat)
	if status != 200 || !strings.Contains(body, "FROM_ADMIN_APIKEY") {
		t.Fatalf("chat via api-key account: status = %d, body = %s", status, body)
	}

	// 空 api_key PUT：沿用旧值
	status, body = adminDo(t, "PUT", gw.URL+"/admin/accounts/a1",
		`{"type":"api-key","protocol":"anthropic","models":{"claude-sonnet-4-5":"native-model"}}`, adminKey)
	if status != 200 || !strings.Contains(body, `"api_key":"****abcd"`) {
		t.Fatalf("put empty api_key: status = %d, body = %s", status, body)
	}
	// 沿用后凭据仍可用
	if status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat); status != 200 || !strings.Contains(body, "FROM_ADMIN_APIKEY") {
		t.Fatalf("chat after put: status = %d, body = %s", status, body)
	}
	if a := kiroAccountByName(m, "a1"); a.Type != account.TypeAPIKey {
		t.Errorf("a1 type = %q", a.Type)
	}
}

// kiro 运维端点：refresh / test / usage；/admin/models 并集（预热后轮询）。
func TestE2EAdminKiroOpsAndModels(t *testing.T) {
	gw, _, st := newAdminEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroChatStream("ADMIN_OPS"))
	})
	if status, body := adminDo(t, "POST", gw.URL+"/admin/accounts", kiroCreateBody, adminKey); status != 201 {
		t.Fatalf("create: status = %d, body = %s", status, body)
	}
	// 触发一次成功请求：初始化 token + 启动模型预热
	if status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat); status != 200 || !strings.Contains(body, "ADMIN_OPS") {
		t.Fatalf("warmup chat: status = %d, body = %s", status, body)
	}

	// refresh：强刷 token
	refreshBefore := st.refresh.Load()
	if status, body := adminDo(t, "POST", gw.URL+"/admin/accounts/k1/refresh", "", adminKey); status != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("refresh: status = %d, body = %s", status, body)
	}
	if got := st.refresh.Load(); got <= refreshBefore {
		t.Errorf("refresh not triggered: %d -> %d", refreshBefore, got)
	}

	// test：连通性（模型列表）
	if status, body := adminDo(t, "POST", gw.URL+"/admin/accounts/k1/test", "", adminKey); status != 200 || !strings.Contains(body, `"models":1`) {
		t.Fatalf("test: status = %d, body = %s", status, body)
	}

	// usage：GetUsageLimits 原样透传
	if status, body := adminDo(t, "GET", gw.URL+"/admin/accounts/k1/usage", "", adminKey); status != 200 || !strings.Contains(body, `"resetDate":"2099-01-01"`) {
		t.Fatalf("usage: status = %d, body = %s", status, body)
	}

	// models 并集：kiro 预热是异步的，轮询等待
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, body := adminDo(t, "GET", gw.URL+"/admin/models", "", adminKey)
		if status == 200 && strings.Contains(body, "claude-sonnet-4-5") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("models union missing kiro model: status = %d, body = %s", status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// kiroAccountByName 状态快照里按名取账号。
func kiroAccountByName(m *account.Manager, name string) account.Account {
	for _, a := range m.Status() {
		if a.Name == name {
			return a
		}
	}
	panic(name + " not found")
}

// 内联凭据 e2e：text/b64 建号即调度走通 chat、响应脱敏、互斥/形态 400、
// 空内联 PUT 沿用旧值。
func TestE2EAdminInlineCreds(t *testing.T) {
	gw, _, st := newAdminEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(200)
		_, _ = w.Write(kiroChatStream("ADMIN_INLINE"))
	})
	credsB64 := base64.StdEncoding.EncodeToString([]byte(
		`{"refreshToken":"rt-inline-file","region":"us-east-1"}`))

	// text 形态（裸串）+ b64 形态（credentials.json 原文）建号
	for _, tc := range []struct{ name, body string }{
		{"k-text", `{"name":"k-text","type":"kiro","kiro":{"source":"refresh_token","creds_text":"rt-abcdef123456"}}`},
		{"k-b64", `{"name":"k-b64","type":"kiro","kiro":{"source":"creds_file","creds_b64":"` + credsB64 + `"}}`},
	} {
		status, body := adminDo(t, "POST", gw.URL+"/admin/accounts", tc.body, adminKey)
		if status != 201 {
			t.Fatalf("create %s: status = %d, body = %s", tc.name, status, body)
		}
		if strings.Contains(body, "rt-abcdef123456") || strings.Contains(body, "rt-inline-file") {
			t.Errorf("create %s: raw credential leaked: %s", tc.name, body)
		}
	}

	// 两账号均可调度（内联凭据刷新走 mock）
	if status, body := postChat(t, gw.URL, "/v1/messages", anthropicChat); status != 200 || !strings.Contains(body, "ADMIN_INLINE") {
		t.Fatalf("chat via inline account: status = %d, body = %s", status, body)
	}
	if got := st.chat.Load(); got < 1 {
		t.Errorf("kiro chat called %d times", got)
	}

	// 列表脱敏含内联字段
	status, body := adminDo(t, "GET", gw.URL+"/admin/accounts", "", adminKey)
	if status != 200 || !strings.Contains(body, `"creds_text":"****3456"`) {
		t.Errorf("list: inline creds not masked: status = %d, body = %s", status, body)
	}
	if strings.Contains(body, "rt-inline-file") {
		t.Errorf("list: raw creds_b64 leaked")
	}

	// 校验矩阵 400：互斥 / 非法 base64 / 非法内容
	for _, tc := range []struct{ body, want string }{
		{`{"name":"x","type":"kiro","kiro":{"source":"refresh_token","refresh_token":"rt","creds_text":"rt2"}}`, "conflict"},
		{`{"name":"x","type":"kiro","kiro":{"source":"refresh_token","creds_b64":"!!!"}}`, "invalid base64"},
		{`{"name":"x","type":"kiro","kiro":{"source":"creds_file","creds_text":"not json"}}`, "not valid JSON"},
		{`{"name":"x","type":"kiro","kiro":{"source":"cli_db","creds_b64":"` + base64.StdEncoding.EncodeToString([]byte("garbage")) + `"}}`, "not a SQLite database"},
	} {
		if status, body := adminDo(t, "POST", gw.URL+"/admin/accounts", tc.body, adminKey); status != 400 || !strings.Contains(body, tc.want) {
			t.Errorf("validate %q: status = %d, body = %s, want 400 mentioning %q", tc.body, status, body, tc.want)
		}
	}

	// 空内联 PUT：沿用旧值（k-text 凭据仍有效）
	status, body = adminDo(t, "PUT", gw.URL+"/admin/accounts/k-text",
		`{"type":"kiro","kiro":{"source":"refresh_token"}}`, adminKey)
	if status != 200 || !strings.Contains(body, `"creds_text":"****3456"`) {
		t.Fatalf("put empty inline: status = %d, body = %s", status, body)
	}
	if strings.Contains(body, "rt-abcdef123456") {
		t.Errorf("put response leaked raw credential")
	}
}
