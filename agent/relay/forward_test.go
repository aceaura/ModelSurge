package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
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

func TestEndpointRejectsKiroGeminiAndUnknownProtocols(t *testing.T) {
	for _, protocol := range []string{"kiro", "gemini", "unknown"} {
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type kiroExecuteReplay struct {
	lease        replayv1.TargetLease
	executeCalls atomic.Int64
	body         io.ReadCloser
	executeReq   replayv1.KiroExecuteRequest
}

func (r *kiroExecuteReplay) Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	return r.lease, nil
}
func (*kiroExecuteReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{Applied: true}, nil
}
func (r *kiroExecuteReplay) ExecuteKiro(_ context.Context, req replayv1.KiroExecuteRequest) (*http.Response, error) {
	r.executeCalls.Add(1)
	r.executeReq = req
	if r.body != nil {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/x-ndjson"}}, Body: r.body}, nil
	}
	events := []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "from replay"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 3}},
		{Type: ir.EvMessageStop},
	}
	var body bytes.Buffer
	for _, event := range events {
		_ = json.NewEncoder(&body).Encode(event)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/x-ndjson"}}, Body: io.NopCloser(bytes.NewReader(body.Bytes()))}, nil
}
func (*kiroExecuteReplay) WebSearch(context.Context, replayv1.WebSearchRequest) (replayv1.WebSearchResponse, error) {
	return replayv1.WebSearchResponse{}, nil
}

func TestAgentDataPlaneLogsUseSameRequestIDAndRedactSecrets(t *testing.T) {
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	replay := &kiroExecuteReplay{lease: replayv1.TargetLease{RequestID: "lease-id", GroupID: "group", TargetID: "kiro/public", Protocol: "kiro"}}
	f := NewForwarder(&config.Config{AccessLogEnabled: true}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{Model: "public", Stream: true, Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "private-message"}}}}}, "client-secret-key")
	if replay.executeReq.RequestID == "" {
		t.Fatal("Kiro request_id was not propagated")
	}
	got := logs.String()
	for _, phase := range []string{"phase=request_in", "phase=dispatch_out", "phase=dispatch_in", "phase=upstream_out", "phase=upstream_in", "phase=kiro_stream_done", "phase=client_out"} {
		if !strings.Contains(got, phase+" request_id="+replay.executeReq.RequestID) {
			t.Errorf("missing correlated %s in logs: %s", phase, got)
		}
	}
	for _, forbidden := range []string{"client-secret-key", "private-message", "Authorization", "access-token"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("logs leaked %q: %s", forbidden, got)
		}
	}
}

func TestKiroStrictPolicyLogsAggregatedUpstreamAndClientFrames(t *testing.T) {
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	replay := &kiroExecuteReplay{lease: replayv1.TargetLease{GroupID: "group", TargetID: "kiro/public", Protocol: "kiro"}}
	forwarder := NewForwarder(&config.Config{AccessLogEnabled: true}, replay, nil)
	forwarder.Forward(t.Context(), httptest.NewRecorder(), proto.MustInbound("anthropic"), &ir.Request{
		Model: "public", Stream: true, ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceNone},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "private-message"}}}},
	}, "client-secret-key")

	got := logs.String()
	for _, phase := range []string{"phase=upstream_stream_done request_id=" + replay.executeReq.RequestID, "phase=client_out request_id=" + replay.executeReq.RequestID} {
		if !strings.Contains(got, phase) {
			t.Errorf("missing %s: %s", phase, got)
		}
	}
	if strings.Contains(got, "frames=0") {
		t.Fatalf("strict streaming response logged zero frames: %s", got)
	}
	for _, forbidden := range []string{"client-secret-key", "private-message"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("logs leaked %q: %s", forbidden, got)
		}
	}
}

func TestKiroLeaseUsesReplayExecuteInsteadOfDirectHTTP(t *testing.T) {
	replay := &kiroExecuteReplay{lease: replayv1.TargetLease{RequestID: "req", GroupID: "group", TargetID: "kiro/public", Protocol: "kiro"}}
	f := NewForwarder(&config.Config{}, replay, nil)
	var directCalls atomic.Int64
	f.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		directCalls.Add(1)
		return nil, fmt.Errorf("direct HTTP must not be used")
	})}
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{Model: "public", Stream: true, Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}, "client-key")
	if replay.executeCalls.Load() != 1 || directCalls.Load() != 0 {
		t.Fatalf("execute calls=%d direct calls=%d", replay.executeCalls.Load(), directCalls.Load())
	}
	if !strings.Contains(w.Body.String(), "from replay") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestKiroStreamFlushesTextBeforeUpstreamCloses(t *testing.T) {
	reader, writer := io.Pipe()
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() {
		encoder := json.NewEncoder(writer)
		for _, event := range []ir.Event{
			{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
			{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
			{Type: ir.EvTextDelta, Index: 0, Text: "early text"},
		} {
			if err := encoder.Encode(event); err != nil {
				return
			}
		}
		<-release
		for _, event := range []ir.Event{
			{Type: ir.EvBlockStop, Index: 0},
			{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
			{Type: ir.EvMessageStop},
		} {
			if err := encoder.Encode(event); err != nil {
				return
			}
		}
		_ = writer.Close()
	}()

	replay := &kiroExecuteReplay{
		lease: replayv1.TargetLease{RequestID: "req", GroupID: "group", TargetID: "kiro/public", Protocol: "kiro"},
		body:  reader,
	}
	forwarder := NewForwarder(&config.Config{}, replay, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarder.Forward(r.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
			Model: "public", Stream: true,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		}, "client-key")
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	found := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "early text") {
				found <- true
				return
			}
		}
		found <- false
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Fatal("stream ended before the first text delta")
		}
	case <-time.After(time.Second):
		t.Fatal("first text delta was buffered until upstream completion")
	}
	close(release)
}

func TestDispatchFailureLogsClientErrorWithoutSecrets(t *testing.T) {
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	replay := &rejectingLeaseReplay{}
	forwarder := NewForwarder(&config.Config{AccessLogEnabled: true}, replay, nil)
	forwarder.Forward(t.Context(), httptest.NewRecorder(), proto.MustInbound("anthropic"), &ir.Request{Model: "public"}, "client-secret-key")
	got := logs.String()
	if !strings.Contains(got, "phase=client_out request_id=") || !strings.Contains(got, "error=true") {
		t.Fatalf("missing client error summary: %s", got)
	}
	if strings.Contains(got, "client-secret-key") {
		t.Fatalf("logs leaked client key: %s", got)
	}
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
