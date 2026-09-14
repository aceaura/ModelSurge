package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// 账号级自定义头须并入协议默认头，同名键覆盖协议值（如网关会话头）。
func TestStaticEndpointExtraHeaders(t *testing.T) {
	resolve, err := staticEndpoint("openai-chat", "https://gw.example/zen/go", "sk-test", "m1",
		map[string]string{"x-opencode-session": "sess-1", "Authorization": "Bearer override"})
	if err != nil {
		t.Fatalf("static endpoint: %v", err)
	}
	url, headers, rerr := resolve(t.Context())
	if rerr != nil {
		t.Fatalf("resolve: %v", rerr)
	}
	if url != "https://gw.example/zen/go/v1/chat/completions" {
		t.Fatalf("url = %q", url)
	}
	if headers["x-opencode-session"] != "sess-1" {
		t.Fatalf("x-opencode-session = %q", headers["x-opencode-session"])
	}
	if headers["Authorization"] != "Bearer override" {
		t.Fatalf("Authorization = %q, want override", headers["Authorization"])
	}
	if headers["Content-Type"] != "" { // 协议默认头不该被 extra 清掉
		t.Fatalf("protocol headers lost: %v", headers)
	}

	// 无 extra：行为与原来一致
	resolve2, err := staticEndpoint("anthropic", "https://up.example", "sk-up", "m1", nil)
	if err != nil {
		t.Fatalf("static endpoint: %v", err)
	}
	url2, headers2, _ := resolve2(t.Context())
	if url2 != "https://up.example/v1/messages" || headers2["x-api-key"] != "sk-up" {
		t.Fatalf("plain endpoint changed: %q %v", url2, headers2)
	}
}

// codex 协议：无 /v1 的 /responses 路径 + 配套身份头；账号 Headers 覆盖默认值。
func TestCodexEndpoint(t *testing.T) {
	resolve, err := staticEndpoint("codex", "https://chatgpt.com/backend-api/codex", "eyJtok", "gpt-5.6-sol",
		map[string]string{"chatgpt-account-id": "acc-123"})
	if err != nil {
		t.Fatalf("static endpoint: %v", err)
	}
	url, headers, rerr := resolve(t.Context())
	if rerr != nil {
		t.Fatalf("resolve: %v", rerr)
	}
	if url != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("url = %q", url)
	}
	if headers["Authorization"] != "Bearer eyJtok" {
		t.Fatalf("Authorization = %q", headers["Authorization"])
	}
	if headers["originator"] != "codex-tui" || headers["version"] == "" || headers["OpenAI-Beta"] != "responses=experimental" {
		t.Fatalf("codex identity headers missing: %v", headers)
	}
	if ua := headers["User-Agent"]; len(ua) < len("codex-tui/") || ua[:10] != "codex-tui/" {
		t.Fatalf("User-Agent = %q, want codex-tui/<version> ...", ua)
	}
	if headers["session_id"] == "" {
		t.Fatalf("session_id missing")
	}
	// 每次解析都生成新 session_id（每请求隔离，对齐 sub2api 语义）
	resolve3, err := staticEndpoint("codex", "https://chatgpt.com/backend-api/codex", "t", "m", nil)
	if err != nil {
		t.Fatalf("static endpoint: %v", err)
	}
	_, headers3, _ := resolve3(t.Context())
	if headers3["session_id"] == headers["session_id"] {
		t.Fatalf("session_id should be per-request unique: %q", headers3["session_id"])
	}
	if headers3["chatgpt-account-id"] != "" { // 未配置时不强造
		t.Fatalf("chatgpt-account-id should be absent unless configured")
	}

	// base_url 缺省兜底官方订阅端点
	resolve4, err := staticEndpoint("codex", "", "t", "m", nil)
	if err != nil {
		t.Fatalf("static endpoint: %v", err)
	}
	url4, _, _ := resolve4(t.Context())
	if url4 != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("default base url = %q", url4)
	}
}

func TestEndpointRejectsGeminiAndUnknownProtocols(t *testing.T) {
	for _, protocol := range []string{"gemini", "unknown"} {
		if _, _, err := endpoint(protocol, "https://provider.test", "key", "model"); err == nil {
			t.Fatalf("endpoint(%q) unexpectedly succeeded", protocol)
		}
		if _, err := staticEndpoint(protocol, "https://provider.test", "key", "model", nil); err == nil {
			t.Fatalf("staticEndpoint(%q) unexpectedly succeeded", protocol)
		}
	}
}

type rejectingLeaseReplay struct {
	lease replayv1.TargetLease
	calls atomic.Int64
}

func (r *rejectingLeaseReplay) Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	if r.calls.Add(1) == 1 {
		return r.lease, nil
	}
	return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeTargetUnavailable, Message: "no target"}
}
func (*rejectingLeaseReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{}, nil
}
func (*rejectingLeaseReplay) WebSearch(context.Context, replayv1.WebSearchRequest) (replayv1.WebSearchResponse, error) {
	return replayv1.WebSearchResponse{}, nil
}

func TestGeminiLeaseRejectedWithoutProviderConnection(t *testing.T) {
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls.Add(1)
	}))
	defer provider.Close()

	replay := &rejectingLeaseReplay{lease: replayv1.TargetLease{
		TargetID: "gemini-target", Protocol: "gemini", NativeModel: "gemini-pro",
		BaseURL: provider.URL, Credential: "secret",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("gemini"), &ir.Request{Model: "public"}, "client-key")
	if providerCalls.Load() != 0 {
		t.Fatalf("provider calls=%d, want 0", providerCalls.Load())
	}
	if replay.calls.Load() != 2 {
		t.Fatalf("dispatch calls=%d, want 2", replay.calls.Load())
	}
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("expected Gemini-shaped error, got %s", w.Body.String())
	}
}
