package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// host 分流：有 profileArn 走 runtime；免费账号（无 ARN）回落 q；控制面恒 q。
func TestHostRouting(t *testing.T) {
	oldRT, oldQT := runtimeHostTemplate, qHostTemplate
	runtimeHostTemplate, qHostTemplate = "https://rt-%s.kiro.test", "https://q-%s.aws.test"
	t.Cleanup(func() { runtimeHostTemplate, qHostTemplate = oldRT, oldQT })

	// 付费账号（有 profileArn）
	paid := NewAuthService(nil, "paid", &KiroAccount{
		Source: SourceRefreshToken, RefreshToken: "rt", APIRegion: "us-east-1",
		Token: &TokenState{ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/p"},
	})
	if got := paid.ChatHost(); got != "https://rt-us-east-1.kiro.test" {
		t.Errorf("paid ChatHost = %s", got)
	}
	if got := paid.ControlPlaneHost(); got != "https://q-us-east-1.aws.test" {
		t.Errorf("ControlPlaneHost = %s", got)
	}

	// 免费账号（无 profileArn）：聊天回落 q
	free := NewAuthService(nil, "free", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt"})
	if got := free.ChatHost(); got != "https://q-us-east-1.aws.test" {
		t.Errorf("free ChatHost = %s, want q fallback", got)
	}

	// API 区覆盖链：账号级 > 环境变量 > 检测 > SSO > 默认
	acc := NewAuthService(nil, "r", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt", Region: "eu-central-1"})
	if got := acc.EffectiveAPIRegion(); got != "eu-central-1" {
		t.Errorf("sso-region fallback = %s, want eu-central-1", got)
	}
	t.Setenv("KIRO_API_REGION", "ap-south-1")
	if got := acc.EffectiveAPIRegion(); got != "ap-south-1" {
		t.Errorf("env override = %s, want ap-south-1", got)
	}
	acc2 := NewAuthService(nil, "r2", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt", Region: "eu-central-1", APIRegion: "us-west-2"})
	if got := acc2.EffectiveAPIRegion(); got != "us-west-2" {
		t.Errorf("account override = %s, want us-west-2 (beats env)", got)
	}
}

// KiroHeaders：UA 指纹、x-amz-target、随机 invocation id。
func TestKiroHeaders(t *testing.T) {
	h := KiroHeaders("fp123", "tok", TargetGenerateAssistantResponse)
	if h["Authorization"] != "Bearer tok" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
	if h["x-amz-target"] != TargetGenerateAssistantResponse {
		t.Errorf("x-amz-target = %q", h["x-amz-target"])
	}
	if h["User-Agent"] != "aws-sdk-js/1.0.27 ua/2.1 os/win32#10.0.19044 lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-0.7.45-fp123" {
		t.Errorf("User-Agent = %q", h["User-Agent"])
	}
	if h["x-amzn-kiro-agent-mode"] != "vibe" {
		t.Errorf("agent mode = %q", h["x-amzn-kiro-agent-mode"])
	}
	h2 := KiroHeaders("fp123", "tok", TargetGenerateAssistantResponse)
	if h["amz-sdk-invocation-id"] == h2["amz-sdk-invocation-id"] {
		t.Errorf("invocation id must be random per request")
	}
}

// clientTestEnv 控制面 mock：替换 host 模板 + 退避归零。
func clientTestEnv(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(func() { srv.Close() })
	oldRT, oldQT := runtimeHostTemplate, qHostTemplate
	runtimeHostTemplate, qHostTemplate = srv.URL+"/rt-%s", srv.URL+"/q-%s"
	t.Cleanup(func() { runtimeHostTemplate, qHostTemplate = oldRT, oldQT })
}

func newTestClient(auth *AuthService) *KiroClient {
	c := NewKiroClient(auth)
	c.retryDelay = time.Millisecond
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

// 控制面方法 + 重试：5xx 退避重试后成功；403 强刷后成功；耗尽后报错。
func TestKiroClientRetry(t *testing.T) {
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accessToken":"at-ok","expiresIn":3600}`)
	})
	var calls int32
	clientTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.WriteHeader(500)
		case 2:
			w.WriteHeader(503)
		case 3:
			fmt.Fprint(w, `{"models":[{"modelId":"claude-sonnet-4-5","modelName":"Claude Sonnet 4.5"}]}`)
		default:
			fmt.Fprint(w, `{"models":[]}`)
		}
	})
	auth := NewAuthService(nil, "k", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt"})
	models, err := newTestClient(auth).ListAvailableModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ModelID != "claude-sonnet-4-5" {
		t.Fatalf("models = %v err = %v", models, err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("calls = %d, want 3 (two 5xx then success)", calls)
	}
}

func TestKiroClient403ForceRefresh(t *testing.T) {
	// 刷新端点 mock（desktop）
	var refreshes int32
	refreshSrv := authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshes, 1)
		fmt.Fprint(w, `{"accessToken":"at-ok","expiresIn":3600}`)
	})
	_ = refreshSrv

	var calls int32
	clientTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(403)
			return
		}
		// 第二次必须带强刷后的新 token
		if r.Header.Get("Authorization") != "Bearer at-ok" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:1:profile/x","profileName":"default"}]}`)
	})
	auth := NewAuthService(nil, "k", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt"})
	arn, err := newTestClient(auth).ListAvailableProfiles(context.Background())
	if err != nil || arn != "arn:aws:codewhisperer:us-east-1:1:profile/x" {
		t.Fatalf("arn = %q err = %v", arn, err)
	}
	if atomic.LoadInt32(&refreshes) != 2 {
		t.Errorf("refreshes = %d, want 2 (initial + 403-forced)", refreshes)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestKiroClientGetUsageLimits(t *testing.T) {
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accessToken":"at-ok","expiresIn":3600}`)
	})
	var body map[string]string
	clientTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-amz-target") != targetGetUsage {
			w.WriteHeader(404)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"usage":{"AGENTIC_REQUEST":{"used":10,"limit":50},"resetDate":"2026-09-13"}}`)
	})
	auth := NewAuthService(nil, "k", &KiroAccount{
		Source: SourceRefreshToken, RefreshToken: "rt",
		Token: &TokenState{ProfileArn: "arn:test"},
	})
	raw, err := newTestClient(auth).GetUsageLimits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if body["profileArn"] != "arn:test" || body["resourceType"] != "AGENTIC_REQUEST" {
		t.Errorf("request body = %v", body)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["resetDate"] != "2026-09-13" {
		t.Errorf("response = %v", resp)
	}

	// 无 profileArn：直接报错（免费账号没有配额画像）
	free := NewAuthService(nil, "free", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt"})
	if _, err := newTestClient(free).GetUsageLimits(context.Background()); err == nil {
		t.Error("usage limits without profileArn must error")
	}
}

// CallWebSearch：MCP JSON-RPC 形状（method/tools/call、无 x-amz-target）、
// 响应 content[0].text 二次解析、错误与 rpc error 透出。
func TestKiroClientCallWebSearch(t *testing.T) {
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accessToken":"at-ok","expiresIn":3600}`)
	})
	var gotReq map[string]any
	clientTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/q-us-east-1/mcp" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("x-amz-target") != "" {
			t.Errorf("mcp request must not carry x-amz-target")
		}
		if r.Header.Get("x-amzn-codewhisperer-optout") != "false" {
			t.Errorf("optout = %q, want false", r.Header.Get("x-amzn-codewhisperer-optout"))
		}
		if r.Header.Get("Authorization") != "Bearer at-ok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		// content[0].text 是 JSON 字符串（嵌套引号）
		fmt.Fprint(w, `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Go\",\"url\":\"https://go.dev\",\"snippet\":\"fast\"}]}"}],"isError":false}}`)
	})
	auth := NewAuthService(nil, "k", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt", APIRegion: "us-east-1"})
	id, results, err := newTestClient(auth).CallWebSearch(context.Background(), "go release notes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "srvtoolu_") {
		t.Errorf("id = %q, want srvtoolu_ prefix", id)
	}
	if len(results) != 1 || results[0].Title != "Go" || results[0].URL != "https://go.dev" || results[0].Snippet != "fast" {
		t.Errorf("results = %+v", results)
	}
	if gotReq["jsonrpc"] != "2.0" || gotReq["method"] != "tools/call" {
		t.Errorf("mcp request = %v", gotReq)
	}
	params := gotReq["params"].(map[string]any)
	if params["name"] != "web_search" {
		t.Errorf("params = %v", params)
	}
	if args := params["arguments"].(map[string]any); args["query"] != "go release notes" {
		t.Errorf("arguments = %v", args)
	}
	if id2, _, _ := newTestClient(auth).CallWebSearch(context.Background(), "x"); id2 == id {
		t.Errorf("ids must be random per call")
	}
}

// CallWebSearch 错误路径：非 200 透出错误。
func TestKiroClientCallWebSearchErrors(t *testing.T) {
	authTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"accessToken":"at-ok","expiresIn":3600}`)
	})
	clientTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/q-us-east-1/mcp" {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(404)
	})
	auth := NewAuthService(nil, "k", &KiroAccount{Source: SourceRefreshToken, RefreshToken: "rt", APIRegion: "us-east-1"})
	_, _, err := newTestClient(auth).CallWebSearch(context.Background(), "q")
	if err == nil {
		t.Fatal("503 must error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v", err)
	}
}
