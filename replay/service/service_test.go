package service

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/aceaura/ModelSurge/upstream/dialect"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/replay/schedule"
	"github.com/aceaura/ModelSurge/upstream/contract/upstreamv1"
	upstreamir "github.com/aceaura/ModelSurge/upstream/ir"
)

type fakeUpstream struct {
	evaluates   int
	resolves    int
	reports     int
	target      upstreamv1.ResolvedTarget
	executeResp *http.Response
	candidates  []upstreamv1.CandidateEvaluation
}

func (f *fakeUpstream) Health(context.Context) error { return nil }
func (f *fakeUpstream) Models(context.Context) ([]upstreamv1.ModelSummary, error) {
	return []upstreamv1.ModelSummary{{ID: "a/model"}}, nil
}
func (f *fakeUpstream) Resolve(_ context.Context, id string) (upstreamv1.ResolvedTarget, error) {
	f.resolves++
	if f.target.Protocol != "" {
		target := f.target
		target.ID = id
		return target, nil
	}
	return upstreamv1.ResolvedTarget{ID: id, Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test", APIKey: "secret"}, nil
}
func (f *fakeUpstream) Evaluate(_ context.Context, ids []string) ([]upstreamv1.CandidateEvaluation, error) {
	f.evaluates++
	if f.candidates != nil {
		return f.candidates, nil
	}
	return []upstreamv1.CandidateEvaluation{{ID: ids[0], Available: true}}, nil
}
func (f *fakeUpstream) Report(context.Context, upstreamv1.ResultReport) (upstreamv1.ResultResponse, error) {
	f.reports++
	return upstreamv1.ResultResponse{Applied: true}, nil
}
func (f *fakeUpstream) ExecuteKiro(context.Context, upstreamv1.KiroExecuteRequest) (*http.Response, error) {
	return f.executeResp, nil
}
func (f *fakeUpstream) WebSearch(context.Context, upstreamv1.WebSearchRequest) (upstreamv1.WebSearchResponse, error) {
	return upstreamv1.WebSearchResponse{ID: "srvtoolu_test"}, nil
}

func newTestServer(t *testing.T) (*HTTPServer, *relaystore.Store, *fakeUpstream) {
	t.Helper()
	store, err := relaystore.Open(dialect.SQLite, filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "public", Protocol: "openai-chat", APIKey: "client-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGroup(ctx, relaystore.Group{ID: "g", UserModel: "public", PolicyType: "sticky", PolicyConfig: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMembers(ctx, "g", []string{"a/model"}); err != nil {
		t.Fatal(err)
	}
	upstream := &fakeUpstream{}
	scheduler := &schedule.Scheduler{Store: store, Upstream: upstream}
	return NewHTTPServer(&Service{Store: store, Scheduler: scheduler, Upstream: upstream}, "agent-key", "admin-key"), store, upstream
}

func TestDispatchLogsAuthSelectEvaluateAndRedactsClientKey(t *testing.T) {
	server, _, _ := newTestServer(t)
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	dispatch := replayv1.DispatchRequest{Model: "public", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-log"}
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusOK, nil)
	got := logs.String()
	for _, phase := range []string{"phase=dispatch_validate", "phase=auth_start", "phase=auth_result", "phase=select_start", "phase=evaluate_out", "phase=evaluate_in", "phase=resolve_out", "phase=resolve_in", "phase=selected"} {
		if !strings.Contains(got, phase+" request_id=req-log") {
			t.Errorf("missing %s: %s", phase, got)
		}
	}
	if strings.Contains(got, "client-key") {
		t.Fatalf("Replay logs leaked ClientKey: %s", got)
	}
}

func TestAccessLogDisabledSilencesReplayLayers(t *testing.T) {
	server, _, _ := newTestServer(t)
	server.SetAccessLog(false)
	server.service.AccessLogConfigured = true
	server.service.AccessLogEnabled = false
	server.service.Scheduler.AccessLogConfigured = true
	server.service.Scheduler.AccessLogEnabled = false

	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	dispatch := replayv1.DispatchRequest{Model: "public", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-silent"}
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusOK, nil)
	if logs.Len() != 0 {
		t.Fatalf("access_log=false emitted logs: %s", logs.String())
	}
}

func TestDispatchAuthenticatesUserModelAndUsesCache(t *testing.T) {
	server, _, upstream := newTestServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	dispatch := replayv1.DispatchRequest{Model: "public", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-1"}
	var first replayv1.TargetLease
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusOK, &first)
	if first.TargetID != "a/model" || first.Credential != "secret" || upstream.evaluates != 1 || upstream.resolves != 1 {
		t.Fatalf("lease=%+v eval=%d resolve=%d", first, upstream.evaluates, upstream.resolves)
	}
	var second replayv1.TargetLease
	dispatch.RequestID = "req-2"
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusOK, &second)
	if upstream.evaluates != 1 || upstream.resolves != 2 {
		t.Fatalf("cache path eval=%d resolve=%d", upstream.evaluates, upstream.resolves)
	}

	dispatch.ClientKey = "wrong"
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusUnauthorized, nil)
}

// 10.3 dispatch 随送 est_tokens：缓存目标窗口不足绕过落评估，候选仍
// 全被窗口排除时返回 context_too_large（HTTP 413）；est_tokens 未送不过滤。
func TestDispatchFiltersByEstTokens(t *testing.T) {
	server, _, upstream := newTestServer(t)
	upstream.target = upstreamv1.ResolvedTarget{Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test", APIKey: "secret", Runtime: upstreamv1.RuntimeMetadata{MaxInputTokens: 64000}}
	upstream.candidates = []upstreamv1.CandidateEvaluation{{ID: "a/model", Available: true, ContextWindow: 64000}}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// est_tokens 未送：不过滤，正常选中（并写缓存）
	dispatch := replayv1.DispatchRequest{Model: "public", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-est-1"}
	var lease replayv1.TargetLease
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusOK, &lease)
	if lease.TargetID != "a/model" {
		t.Fatalf("lease=%+v", lease)
	}

	// est_tokens 超窗口：缓存绕过 → 评估全排除 → context_too_large（413）
	dispatch.RequestID = "req-est-2"
	dispatch.EstTokens = 100000
	resp := postDispatch(t, ts.URL, dispatch)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413", resp.StatusCode)
	}
	var env struct {
		Error replayv1.Error `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != replayv1.CodeContextTooLarge || env.Error.Retryable {
		t.Fatalf("error=%+v, want context_too_large non-retryable", env.Error)
	}
}

func postDispatch(t *testing.T, base string, dispatch replayv1.DispatchRequest) *http.Response {
	t.Helper()
	body, _ := json.Marshal(dispatch)
	req, _ := http.NewRequest(http.MethodPost, base+replayv1.BasePath+"/dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestDispatchRejectsGeminiAndUnknownTargetsButAcceptsGeminiInbound(t *testing.T) {
	for _, outbound := range []string{"gemini", "unknown"} {
		t.Run(outbound, func(t *testing.T) {
			server, store, upstream := newTestServer(t)
			ctx := context.Background()
			if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "gemini-client", Protocol: "gemini", APIKey: "gemini-key", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if err := store.PutGroup(ctx, relaystore.Group{ID: "gemini-group", UserModel: "gemini-client", PolicyType: "sticky", PolicyConfig: "{}"}); err != nil {
				t.Fatal(err)
			}
			if err := store.AddMembers(ctx, "gemini-group", []string{"a/model"}); err != nil {
				t.Fatal(err)
			}
			upstream.target = upstreamv1.ResolvedTarget{Protocol: outbound, NativeModel: "native", BaseURL: "https://example.test", APIKey: "secret"}

			ts := httptest.NewServer(server.Handler())
			defer ts.Close()
			dispatch := replayv1.DispatchRequest{Model: "gemini-client", InboundProtocol: "gemini", ClientKey: "gemini-key", RequestID: "req-" + outbound}
			doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusServiceUnavailable, nil)
			if upstream.resolves != 1 {
				t.Fatalf("resolve calls=%d, want 1", upstream.resolves)
			}
		})
	}
}

func TestResultsAreIdempotentInReplayAndForwardedUpstream(t *testing.T) {
	server, store, upstream := newTestServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	report := replayv1.ResultReport{ReportID: "report-1", RequestID: "req-1", GroupID: "g", TargetID: "a/model", Outcome: "abnormal", Attempt: 1}
	var first replayv1.ResultResponse
	doJSON(t, ts.URL+replayv1.BasePath+"/results", "agent-key", report, http.StatusOK, &first)
	var second replayv1.ResultResponse
	doJSON(t, ts.URL+replayv1.BasePath+"/results", "agent-key", report, http.StatusOK, &second)
	if !first.Applied || second.Applied || upstream.reports != 2 {
		t.Fatalf("first=%v second=%v reports=%d", first.Applied, second.Applied, upstream.reports)
	}
	group, err := store.GroupForModel(context.Background(), "public")
	if err != nil || group.LastResult != "abnormal" {
		t.Fatalf("group=%+v err=%v", group, err)
	}
	var count int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM result_reports`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestLeaseFromKiroTargetReturnsOpaqueHandle(t *testing.T) {
	lease := leaseFromTarget("request", "group", upstreamv1.ResolvedTarget{
		ID: "kiro-2/gpt-5.6-sol", Protocol: "kiro", NativeModel: "secret-native",
		BaseURL: "https://secret.example", APIKey: "secret", Headers: map[string]string{"Authorization": "Bearer secret"},
		RequestOverrides: &upstreamir.Overrides{}, Runtime: upstreamv1.RuntimeMetadata{AccountType: "kiro"},
	})
	if lease.RequestID != "request" || lease.GroupID != "group" || lease.TargetID != "kiro-2/gpt-5.6-sol" || lease.Protocol != "kiro" {
		t.Fatalf("lease identity = %+v", lease)
	}
	if lease.NativeModel != "" || lease.BaseURL != "" || lease.Credential != "" || lease.Headers != nil || lease.RequestOverrides != nil || lease.Runtime != (replayv1.RuntimeMetadata{}) {
		t.Fatalf("Kiro lease exposed runtime details: %+v", lease)
	}
}

func TestLeaseFromTargetPreservesOverrideTriState(t *testing.T) {
	zeroFloat := 0.0
	zeroInt := 0
	target := upstreamv1.ResolvedTarget{
		ID: "target", Protocol: "openai-chat", NativeModel: "native", BaseURL: "https://example.test",
		RequestOverrides: &upstreamir.Overrides{
			Temperature: &zeroFloat,
			TopP:        nil,
			MaxTokens:   &zeroInt,
			Thinking:    &upstreamir.ThinkingOverride{Enabled: false},
		},
	}
	lease := leaseFromTarget("request", "group", target)
	if lease.RequestOverrides == nil {
		t.Fatal("request overrides lost")
	}
	if lease.RequestOverrides.Temperature == nil || *lease.RequestOverrides.Temperature != 0 {
		t.Fatalf("temperature=%v", lease.RequestOverrides.Temperature)
	}
	if lease.RequestOverrides.TopP != nil {
		t.Fatalf("top_p=%v, want nil", lease.RequestOverrides.TopP)
	}
	if lease.RequestOverrides.MaxTokens == nil || *lease.RequestOverrides.MaxTokens != 0 {
		t.Fatalf("max_tokens=%v", lease.RequestOverrides.MaxTokens)
	}
	if lease.RequestOverrides.Thinking == nil || lease.RequestOverrides.Thinking.Enabled {
		t.Fatalf("thinking=%+v", lease.RequestOverrides.Thinking)
	}
}

func TestKiroExecuteProxiesStatusContentTypeAndBody(t *testing.T) {
	server, _, upstream := newTestServer(t)
	const body = `{"error":{"code":"target_unavailable","message":"limited","retryable":true,"status":429}}`
	upstream.executeResp = &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	reqBody, _ := json.Marshal(replayv1.KiroExecuteRequest{TargetID: "a/model", Request: json.RawMessage(`{"Model":"m"}`)})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+replayv1.BasePath+"/kiro/execute", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer agent-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Content-Type") != "application/json" || string(got) != body {
		t.Fatalf("status=%d content-type=%q body=%s", resp.StatusCode, resp.Header.Get("Content-Type"), got)
	}
}

func TestServiceAndAdminKeysAreIndependent(t *testing.T) {
	server, _, _ := newTestServer(t)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+replayv1.BasePath+"/models", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("service status=%v err=%v", status(resp), err)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/user-models", nil)
	req.Header.Set("X-Admin-Key", "agent-key")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin status=%v err=%v", status(resp), err)
	}
}

func doJSON(t *testing.T, url, key string, input any, want int, output any) {
	t.Helper()
	body, _ := json.Marshal(input)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status=%d want=%d", resp.StatusCode, want)
	}
	if output != nil && want >= 200 && want < 300 {
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
}

func status(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// ---- compact-fallback 第一档：CompressOf 放行与 compress_model 契约 ----

// 鉴权通过的 dispatch：lease 携带 compress_model；鉴权失败不携带（4.3）。
func TestDispatchLeaseCarriesCompressModel(t *testing.T) {
	server, store, _ := newTestServer(t)
	ctx := context.Background()
	if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "public", Protocol: "openai-chat", APIKey: "client-key", Enabled: true, CompressModel: "comp"}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	var lease replayv1.TargetLease
	dispatch := replayv1.DispatchRequest{Model: "public", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-cm"}
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key", dispatch, http.StatusOK, &lease)
	if lease.CompressModel != "comp" {
		t.Fatalf("lease compress_model = %q, want comp", lease.CompressModel)
	}
	// 鉴权失败：错误信封不携带 compress_model（未鉴权请求不触发压缩）
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key",
		replayv1.DispatchRequest{Model: "public", InboundProtocol: "openai-chat", ClientKey: "wrong", RequestID: "req-cm2"},
		http.StatusUnauthorized, nil)
}

// dispatch 失败（鉴权通过后）：错误信封携带 compress_model（组容灾穷尽/
// 窗口过滤超限两条失败路径都可得）。
func TestDispatchErrorEnvelopeCarriesCompressModel(t *testing.T) {
	server, store, upstream := newTestServer(t)
	ctx := context.Background()
	if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "public", Protocol: "openai-chat", APIKey: "client-key", Enabled: true, CompressModel: "comp"}); err != nil {
		t.Fatal(err)
	}
	upstream.candidates = []upstreamv1.CandidateEvaluation{{ID: "a/model", Available: false, ExclusionReason: "cooling_down"}}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	var env struct {
		Error replayv1.Error `json:"error"`
	}
	doJSONErr(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key",
		replayv1.DispatchRequest{Model: "public", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-cm3"},
		http.StatusServiceUnavailable, &env)
	if env.Error.CompressModel != "comp" {
		t.Fatalf("error envelope compress_model = %q, want comp", env.Error.CompressModel)
	}
}

// CompressOf 非空：错误 key 也放行（4.2 信任通道）；置空时同 key 被拒。
func TestDispatchCompressOfBypassesKeyCheck(t *testing.T) {
	server, store, _ := newTestServer(t)
	ctx := context.Background()
	if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "comp", Protocol: "openai-chat", APIKey: "comp-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutGroup(ctx, relaystore.Group{ID: "g-comp", UserModel: "comp", PolicyType: "sticky", PolicyConfig: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMembers(ctx, "g-comp", []string{"a/model"}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// compress_of=public：public 的 key（非 comp 的 key）照样放行
	var lease replayv1.TargetLease
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key",
		replayv1.DispatchRequest{Model: "comp", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-cm4", CompressOf: "public"},
		http.StatusOK, &lease)
	if lease.TargetID == "" {
		t.Fatalf("no lease: %+v", lease)
	}
	// 无 compress_of：错误 key 被拒
	doJSON(t, ts.URL+replayv1.BasePath+"/dispatch", "agent-key",
		replayv1.DispatchRequest{Model: "comp", InboundProtocol: "openai-chat", ClientKey: "client-key", RequestID: "req-cm5"},
		http.StatusUnauthorized, nil)
}

// 管理面：compress_model 引用校验（3.2 硬校验；3.3 窗口 best-effort 不阻止）。
func TestAdminUserModelCompressModelValidation(t *testing.T) {
	server, store, _ := newTestServer(t)
	ctx := context.Background()
	if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "comp", Protocol: "openai-chat", APIKey: "comp-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutUserModel(ctx, relaystore.UserModel{Name: "off", Protocol: "openai-chat", APIKey: "k", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	put := func(name string, m relaystore.UserModel) int {
		body, _ := json.Marshal(m)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/admin/user-models/"+name, bytes.NewReader(body))
		req.Header.Set("X-Admin-Key", "admin-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := put("public", relaystore.UserModel{Protocol: "openai-chat", APIKey: "client-key", Enabled: true, CompressModel: "comp"}); code != http.StatusOK {
		t.Fatalf("valid compress_model: status=%d want 200", code)
	}
	if code := put("public", relaystore.UserModel{Protocol: "openai-chat", APIKey: "client-key", Enabled: true, CompressModel: "public"}); code != http.StatusBadRequest {
		t.Fatalf("self reference: status=%d want 400", code)
	}
	if code := put("public", relaystore.UserModel{Protocol: "openai-chat", APIKey: "client-key", Enabled: true, CompressModel: "missing"}); code != http.StatusBadRequest {
		t.Fatalf("unknown reference: status=%d want 400", code)
	}
	if code := put("public", relaystore.UserModel{Protocol: "openai-chat", APIKey: "client-key", Enabled: true, CompressModel: "off"}); code != http.StatusBadRequest {
		t.Fatalf("disabled reference: status=%d want 400", code)
	}

	// GET 回显（3.4）
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/user-models", nil)
	req.Header.Set("X-Admin-Key", "admin-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var models []relaystore.UserModel
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range models {
		if m.Name == "public" {
			found = true
			if m.CompressModel != "comp" {
				t.Fatalf("GET compress_model = %q, want comp", m.CompressModel)
			}
		}
	}
	if !found {
		t.Fatal("public not listed")
	}
}

// doJSONErr 断言错误状态码并解码 error 信封（doJSON 的 4xx 变体）。
func doJSONErr(t *testing.T, url, key string, input any, want int, env any) {
	t.Helper()
	body, _ := json.Marshal(input)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status=%d want=%d", resp.StatusCode, want)
	}
	if env != nil {
		if err := json.NewDecoder(resp.Body).Decode(env); err != nil {
			t.Fatal(err)
		}
	}
}
