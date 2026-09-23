package relay

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// sseFrames 一段最小完整 anthropic SSE 流，正文是 text。
func sseFrames(text string) []string {
	return []string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"m1","model":"native","usage":{"input_tokens":7}}}`,
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + strconv.Quote(text) + `}}`,
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}`,
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		"event: message_stop\ndata: " + `{"type":"message_stop"}`,
	}
}

// sseUpstream 起一个逐帧 flush 的 anthropic SSE 上游。hits 非 nil 时逐次累加，
// 用来数「重试到底打了几个上游」——只看 Dispatch 次数会把「换了目标但没发请求」
// 也算成一次重发。
func sseUpstream(t *testing.T, frames []string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sseLease 指向给定上游的 anthropic 租约。
func sseLease(baseURL string) replayv1.TargetLease {
	return replayv1.TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "anthropic-1",
		Protocol: "anthropic", NativeModel: "native", BaseURL: baseURL, Credential: "sk-up",
	}
}

// logProbeReplay 记下调度请求里带的 request_id：访问日志的关联性断言要用它
// 而不是自己猜一个 id。
type logProbeReplay struct {
	lease     replayv1.TargetLease
	requestID atomic.Value
}

func (r *logProbeReplay) Dispatch(_ context.Context, req replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	r.requestID.Store(req.RequestID)
	return r.lease, nil
}

func (*logProbeReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{Applied: true}, nil
}

func (r *logProbeReplay) id(t *testing.T) string {
	t.Helper()
	id, _ := r.requestID.Load().(string)
	if id == "" {
		t.Fatal("request_id 没有随调度请求传出去")
	}
	return id
}

// captureLogs 把 agent 的日志改道到缓冲区，返回读取函数。
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var logs bytes.Buffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	return logs.String
}

func TestAgentDataPlaneLogsUseSameRequestIDAndRedactSecrets(t *testing.T) {
	readLogs := captureLogs(t)
	up := sseUpstream(t, sseFrames("from upstream"), nil)
	replay := &logProbeReplay{lease: sseLease(up.URL)}
	f := NewForwarder(&config.Config{AccessLogEnabled: true}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "m", MaxTokens: 64, Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "private-message"}}}},
	}, "client-secret-key")

	id := replay.id(t)
	got := readLogs()
	for _, phase := range []string{
		"phase=request_in", "phase=dispatch_out", "phase=dispatch_in",
		"phase=upstream_out", "phase=upstream_in", "phase=upstream_stream_done", "phase=client_out",
	} {
		if !strings.Contains(got, phase+" request_id="+id) {
			t.Errorf("missing correlated %s in logs: %s", phase, got)
		}
	}
	for _, forbidden := range []string{"client-secret-key", "private-message", "Authorization", "sk-up"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("logs leaked %q: %s", forbidden, got)
		}
	}
}

// 严格工具策略（tool_choice=none 一类）会把上游流聚合成一整条再写出，走的
// 是另一组出口；聚合帧与客户端帧的统计同样要落日志，且不得出现 frames=0
// ——那意味着摘要器一个事件都没看到，日志成了空壳。
func TestStrictPolicyLogsAggregatedUpstreamAndClientFrames(t *testing.T) {
	readLogs := captureLogs(t)
	up := sseUpstream(t, sseFrames("aggregated"), nil)
	replay := &logProbeReplay{lease: sseLease(up.URL)}
	forwarder := NewForwarder(&config.Config{AccessLogEnabled: true}, replay, nil)
	forwarder.Forward(t.Context(), httptest.NewRecorder(), proto.MustInbound("anthropic"), &ir.Request{
		Model: "m", MaxTokens: 64, Stream: true, ToolChoice: &ir.ToolChoice{Mode: ir.ChoiceNone},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "private-message"}}}},
	}, "client-secret-key")

	id := replay.id(t)
	got := readLogs()
	for _, phase := range []string{"phase=upstream_stream_done", "phase=client_out"} {
		if !strings.Contains(got, phase+" request_id="+id) {
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

// 上游还在吐字时客户端就必须已经收到：攒到上游关流再一次性写出会把流式
// 退化成非流式，长回答的客户端要等满整个生成时长才看到第一个字。
func TestStreamFlushesTextBeforeUpstreamCloses(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range sseFrames("early text")[:3] { // 到 text_delta 为止
			_, _ = w.Write([]byte(f + "\n\n"))
			w.(http.Flusher).Flush()
		}
		<-release
		for _, f := range sseFrames("early text")[3:] {
			_, _ = w.Write([]byte(f + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(up.Close)

	forwarder := NewForwarder(&config.Config{}, &logProbeReplay{lease: sseLease(up.URL)}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarder.Forward(r.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
			Model: "m", MaxTokens: 64, Stream: true,
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
