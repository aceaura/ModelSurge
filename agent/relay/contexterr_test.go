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
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func TestClassifyContextError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		msg    string
		code   string
		want   bool
	}{
		// 8 组特征各至少一例（含真实上游报文样本）
		{"openai code", 400, "This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.", "", true},
		{"generic code", 413, "context_too_large", "", true},
		{"deepseek code", 400, "Error: input tokens 100000 exceed the context length_exceeded limit", "", true},
		{"anthropic prompt too long", 400, "prompt is too long: 210000 tokens > 200000 maximum", "", true},
		{"conversation too long", 400, "conversation too long: please start a new conversation", "", true},
		{"context window exceeded", 400, "The context window is too large for this request", "", true},
		{"context length exceeded", 400, "your request exceeds the context length limit", "", true},
		{"token limit with context", 400, "this request exceeds the token limit for the context", "", true},
		{"max context length", 400, "max context length reached", "", true},
		// 上游把信号只放在 error.code（R86 起 relay 会把 code 解析进 ir.Error.Code，
		// 不再整段塞进 Message）：message 泛泛也必须认出来。
		{"openai code field only", 400, "Invalid request", "context_length_exceeded", true},
		{"unrelated code", 400, "rejected", "invalid_api_key", false},
		// 负例
		{"auth error", 400, "invalid api key provided", "", false},
		{"unrelated 400", 400, "invalid request: unknown parameter", "", false},
		{"server error", 500, "maximum context length exceeded", "", false},
		{"code on server error", 500, "boom", "context_length_exceeded", false},
		{"ok", 200, "context length is fine", "", false},
		{"empty message", 400, "", "", false},
		{"model output", 400, "model output is not supported in this mode", "", false},
	}
	for _, c := range cases {
		if got := classifyContextError(c.status, c.msg, c.code); got != c.want {
			t.Errorf("%s: classify(%d, %q, %q)=%v, want %v", c.name, c.status, c.msg, c.code, got, c.want)
		}
	}
}

type reportCaptureReplay struct {
	lease       replayv1.TargetLease
	reports     []replayv1.ResultReport
	dispatch    []replayv1.DispatchRequest
	dispatchErr error
}

func (r *reportCaptureReplay) Dispatch(_ context.Context, req replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	r.dispatch = append(r.dispatch, req)
	if r.dispatchErr != nil {
		return replayv1.TargetLease{}, r.dispatchErr
	}
	return r.lease, nil
}
func (r *reportCaptureReplay) Report(_ context.Context, report replayv1.ResultReport) (replayv1.ResultResponse, error) {
	r.reports = append(r.reports, report)
	return replayv1.ResultResponse{}, nil
}

// 超限错误：单次上游调用（无原位重试、不换目标）、outcome=context_exceeded、
// 客户端收到 400 且消息透传。
func TestContextExceededStopsWithoutRetry(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()

	replay := &reportCaptureReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "acct/m1", Protocol: "openai-chat",
		NativeModel: "m1", BaseURL: upstream.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "public", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1 (no in-place retry, no switch)", upstreamCalls.Load())
	}
	if len(replay.reports) != 1 {
		t.Fatalf("reports=%d, want 1", len(replay.reports))
	}
	if got := replay.reports[0].Outcome; got != ReasonContextExceeded {
		t.Fatalf("outcome=%q, want %q", got, ReasonContextExceeded)
	}
	if got := replay.reports[0].Reason; got != ReasonContextExceeded {
		t.Fatalf("reason=%q, want %q", got, ReasonContextExceeded)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("client status=%d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "maximum context length") {
		t.Fatalf("client body missing upstream message: %s", w.Body.String())
	}
}

// 上游把超限信号只放在 error.code（message 是一句泛泛的 "Invalid request"）时也必须
// 认出来。R86 起 relay 会把上游错误体解析进 ir.Error.Code、不再整段塞进 Message，
// 只看 Message 的分类会因此漏判：outcome 退成 abnormal，Upstream 就不会把这次失败
// 当作「请求本身超窗」处理，反而可能按上游抖动熔断账号。
func TestContextExceededDetectedFromUpstreamCodeOnly(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid request","type":"invalid_request_error","param":null,"code":"context_length_exceeded"}}`))
	}))
	defer upstream.Close()

	replay := &reportCaptureReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "acct/m1", Protocol: "openai-chat",
		NativeModel: "m1", BaseURL: upstream.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "public", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1（超限不可重试）", upstreamCalls.Load())
	}
	if len(replay.reports) != 1 {
		t.Fatalf("reports=%d, want 1", len(replay.reports))
	}
	if got := replay.reports[0].Outcome; got != ReasonContextExceeded {
		t.Fatalf("outcome=%q, want %q（信号在 error.code 里，只扫 message 会漏）", got, ReasonContextExceeded)
	}
	if got := replay.reports[0].Reason; got != ReasonContextExceeded {
		t.Fatalf("reason=%q, want %q", got, ReasonContextExceeded)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("client status=%d, want 400", w.Code)
	}
}

// 普通错误不受影响：abnormal 上报、正常失败语义（回归对照）。
func TestNonContextErrorStillReportsAbnormal(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key provided","type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()

	replay := &reportCaptureReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "acct/m1", Protocol: "openai-chat",
		NativeModel: "m1", BaseURL: upstream.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "public", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if len(replay.reports) != 1 {
		t.Fatalf("reports=%d, want 1", len(replay.reports))
	}
	if got := replay.reports[0].Outcome; got != "abnormal" {
		t.Fatalf("outcome=%q, want abnormal", got)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("client status=%d, want 400", w.Code)
	}
	_ = upstreamCalls.Load()
}

// 9.3 dispatch 随送估算 token：EstTokens = EstimateRequestTokens + MaxTokens。
func TestDispatchSendsEstTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	replay := &reportCaptureReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "acct/m1", Protocol: "openai-chat",
		NativeModel: "m1", BaseURL: upstream.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	req := &ir.Request{
		Model: "public", MaxTokens: 4096,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: strings.Repeat("token ", 2000)}}}},
	}
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), req, "client-key")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(replay.dispatch) != 1 {
		t.Fatalf("dispatches=%d, want 1", len(replay.dispatch))
	}
	want := ir.EstimateRequestTokens(req) + req.MaxTokens
	if got := replay.dispatch[0].EstTokens; got != want || got <= req.MaxTokens {
		t.Fatalf("est_tokens=%d, want %d (estimate+max_tokens)", got, want)
	}
}

// 9.3 replay 报 context_too_large：客户端映射 413（invalid_request）。
func TestReplayDispatchErrorContextTooLarge(t *testing.T) {
	e := replayDispatchError(replayv1.Error{Code: replayv1.CodeContextTooLarge, Message: "estimated 300000 tokens exceed all candidate context windows"})
	if e.StatusCode != http.StatusRequestEntityTooLarge || e.Type != ir.ErrTypeInvalidReq {
		t.Fatalf("mapped=%+v, want 413 invalid_request", e)
	}

	// 端到端：dispatch 失败直通客户端
	replay := &reportCaptureReplay{dispatchErr: replayv1.Error{Code: replayv1.CodeContextTooLarge, Message: "estimated 300000 tokens exceed all candidate context windows"}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "public", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("client status=%d, want 413", w.Code)
	}
	if !strings.Contains(w.Body.String(), "context windows") {
		t.Fatalf("client body=%s", w.Body.String())
	}
}
