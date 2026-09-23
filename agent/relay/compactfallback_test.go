package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// compactReplay 显式压缩回退测试桩：按 dispatch 的 Model 返回不同 lease，
// 记录每次 dispatch（model/compress_of）；orig lease 携带 compress_model。
type compactReplay struct {
	mu          sync.Mutex
	origModel   string
	origLease   replayv1.TargetLease
	compLease   replayv1.TargetLease
	compMissing bool // compress_model dispatch 返回 not_found（引用不存在/禁用）
	dispatches  []replayv1.DispatchRequest
	reports     []replayv1.ResultReport
}

func (r *compactReplay) Dispatch(_ context.Context, d replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dispatches = append(r.dispatches, d)
	// 单目标组：该目标已被 tried = 组容灾穷尽（真实 Replay 语义），
	// 否则 switch_target 后的 redispatch 永远拿到同一 lease。
	exhausted := func(targetID string) bool {
		for _, id := range d.TriedIDs {
			if id == targetID {
				return true
			}
		}
		return false
	}
	if d.Model == r.origModel {
		if exhausted(r.origLease.TargetID) {
			return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeTargetUnavailable, Message: "no available target", Retryable: true}
		}
		lease := r.origLease
		lease.CompressModel = "comp"
		return lease, nil
	}
	if d.Model == "comp" {
		if exhausted(r.compLease.TargetID) {
			return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeTargetUnavailable, Message: "no available target", Retryable: true}
		}
		if r.compMissing {
			return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeNotFound, Message: "user model not found"}
		}
		return r.compLease, nil
	}
	return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeNotFound, Message: "user model not found"}
}

func (r *compactReplay) Report(_ context.Context, report replayv1.ResultReport) (replayv1.ResultResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, report)
	return replayv1.ResultResponse{}, nil
}

func (r *compactReplay) dispatched() []replayv1.DispatchRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]replayv1.DispatchRequest(nil), r.dispatches...)
}

// leaseFor 用 base URL 构造 lease（两个模型各指向一个上游）。
func leaseFor(target, baseURL string) replayv1.TargetLease {
	return replayv1.TargetLease{RequestID: "req", GroupID: "g-" + target, TargetID: target, Protocol: "openai-chat", NativeModel: "native", BaseURL: baseURL, Credential: "sk-up"}
}

// 显式压缩请求：原模型超限（context_exceeded）→ 换 compress_model 重发成功。
// 断言：两次 dispatch（第二次带 CompressOf=原模型名）、客户端 200（1.3）。
func TestExplicitCompactFallsBackOnContextExceeded(t *testing.T) {
	var origCalls, compCalls int
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		origCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		compCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"summarized"},"finish_reason":"stop"}]}`))
	}))
	defer comp.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL), compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "orig", Compact: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "summarized") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if origCalls != 1 || compCalls != 1 {
		t.Fatalf("origCalls=%d compCalls=%d, want 1/1", origCalls, compCalls)
	}
	ds := replay.dispatched()
	if len(ds) != 2 {
		t.Fatalf("dispatches=%d, want 2: %+v", len(ds), ds)
	}
	if ds[0].Model != "orig" || ds[0].CompressOf != "" {
		t.Fatalf("first dispatch: model=%q compress_of=%q", ds[0].Model, ds[0].CompressOf)
	}
	if ds[1].Model != "comp" || ds[1].CompressOf != "orig" {
		t.Fatalf("second dispatch: model=%q compress_of=%q, want comp/orig", ds[1].Model, ds[1].CompressOf)
	}
}

// 普通请求（非显式压缩）：超限不触发回退（第一档仅显式压缩；零回归）。
func TestPlainRequestContextExceededDoesNotFallBack(t *testing.T) {
	var origCalls int
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		origCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model:    "orig",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
	if origCalls != 1 || len(replay.dispatched()) != 1 {
		t.Fatalf("origCalls=%d dispatches=%d, want 1/1 (no fallback)", origCalls, len(replay.dispatched()))
	}
}

// 重试失败：按最后一次失败原样返回（1.4 不吞错误）。
func TestExplicitCompactRetryFailureReturnsLastError(t *testing.T) {
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"compress upstream boom","type":"server_error"}}`))
	}))
	defer comp.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL), compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "orig", Compact: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	// 压缩模型 500 → retryable → 组内无其他候选 → dispatch 失败 → 按最后失败返回
	if w.Code != http.StatusBadGateway && w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 502/503 (last failure)", w.Code)
	}
	if len(replay.dispatched()) < 2 {
		t.Fatalf("dispatches=%d, want >=2", len(replay.dispatched()))
	}
}

// compress_model 引用不存在/禁用：视为未配置 + 告警日志，按原失败返回（1.5）。
func TestExplicitCompactMissingCompressModelReturnsOriginalError(t *testing.T) {
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL), compMissing: true}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "orig", Compact: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (original failure)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "maximum context length") {
		t.Fatalf("body missing original error: %s", w.Body.String())
	}
}

// 递归防护（4.1）：压缩调用自身超限不再触发压缩（深度封顶 1）。
func TestCompressOfDispatchDoesNotRecurse(t *testing.T) {
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer comp.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL), compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "orig", Compact: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	ds := replay.dispatched()
	compDispatches := 0
	for _, d := range ds {
		if d.Model == "comp" {
			compDispatches++
		}
	}
	if compDispatches != 1 {
		t.Fatalf("comp dispatches=%d, want 1 (no recursion)", compDispatches)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
}

// dispatch 失败路径（组容灾穷尽）：显式压缩请求仍可兜底（1.3 模型不可用）。
func TestExplicitCompactFallsBackOnDispatchUnavailable(t *testing.T) {
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"fallback-ok"},"finish_reason":"stop"}]}`))
	}))
	defer comp.Close()

	// orig dispatch 恒失败（target_unavailable，信封带 compress_model）
	replay := &dispatchFailReplay{model: "orig", compressModel: "comp", compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "orig", Compact: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fallback-ok") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if replay.failed != 1 {
		t.Fatalf("orig dispatch failures=%d, want 1", replay.failed)
	}
}

// dispatchFailReplay 原模型 dispatch 恒失败（组容灾穷尽模拟），
// compress_model dispatch 成功。
type dispatchFailReplay struct {
	model         string
	compressModel string
	compLease     replayv1.TargetLease
	failed        int
	dispatches    []replayv1.DispatchRequest
}

func (r *dispatchFailReplay) Dispatch(_ context.Context, d replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	r.dispatches = append(r.dispatches, d)
	if d.Model == r.model {
		r.failed++
		return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeTargetUnavailable, Message: "no available target", Retryable: true, CompressModel: r.compressModel}
	}
	if d.CompressOf != "" {
		return r.compLease, nil
	}
	return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeNotFound, Message: "user model not found"}
}

func (*dispatchFailReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{}, nil
}

// Compact 标记经 Clone 保留（重试路径 req.Clone 语义）。
func TestCompactFlagSurvivesClone(t *testing.T) {
	req := &ir.Request{Model: "m", Compact: true, Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	if c := req.Clone(); !c.Compact {
		t.Fatal("Compact lost after Clone")
	}
}

// Compact 不进上游请求体（codec 自建结构体；保护 IR 新字段不泄漏到出站编码）。
func TestCompactFlagNotInUpstreamRequest(t *testing.T) {
	oc, err := proto.GetOutbound("openai-chat")
	if err != nil {
		t.Fatal(err)
	}
	req := &ir.Request{Model: "m", Compact: true, MaxTokens: 16,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}}}
	body, err := oc.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["Compact"]; ok {
		t.Fatalf("Compact leaked into upstream request: %s", body)
	}
}
