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

func autoMsgs() []ir.Message {
	return []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q1"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "a1"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q2"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "a2"}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "final question"}}},
	}
}

// 第二档全链路（2.1/2.2/2.6/8.5）：普通请求超限 → 压缩调用（compress_model
// + CompressOf）→ 新历史重估 → 重新 dispatch 原模型成功 → 响应带标记 header。
func TestAutoCompactEndToEnd(t *testing.T) {
	var origCalls int32
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&origCalls, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"final-ok"},"finish_reason":"stop"}]}`))
	}))
	defer orig.Close()
	var compCalls int32
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&compCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-sum","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"summarized-history"},"finish_reason":"stop"}]}`))
	}))
	defer comp.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL), compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model:    "orig",
		Messages: autoMsgs(),
	}, "client-key")

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "final-ok") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ModelSurge-Compacted"); got != "true" {
		t.Fatalf("X-ModelSurge-Compacted=%q, want true", got)
	}
	ds := replay.dispatched()
	// dispatch 序列：orig（正常）→ comp（压缩，CompressOf=orig）→ orig（续命）
	if len(ds) != 3 {
		t.Fatalf("dispatches=%d, want 3: %+v", len(ds), ds)
	}
	if ds[0].Model != "orig" || ds[0].CompressOf != "" {
		t.Fatalf("first dispatch: %+v", ds[0])
	}
	if ds[1].Model != "comp" || ds[1].CompressOf != "orig" {
		t.Fatalf("compress dispatch: %+v", ds[1])
	}
	if ds[2].Model != "orig" || ds[2].CompressOf != "" {
		t.Fatalf("redispatch: %+v", ds[2])
	}
	if origCalls != 2 || compCalls != 1 {
		t.Fatalf("origCalls=%d compCalls=%d, want 2/1", origCalls, compCalls)
	}
}

// 压缩调用自身失败（2.4/8.4）：按原超限失败返回，不吞错误不换语义。
func TestAutoCompactCompressionCallFails(t *testing.T) {
	var origCalls, compCalls int32
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&origCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&compCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"compress upstream boom","type":"server_error"}}`))
	}))
	defer comp.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL), compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model:    "orig",
		Messages: autoMsgs(),
	}, "client-key")

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "maximum context length") {
		t.Fatalf("status=%d body=%s, want original 400", w.Code, w.Body.String())
	}
	if w.Header().Get("X-ModelSurge-Compacted") != "" {
		t.Fatal("compacted header must not be set on failure")
	}
}

// 仍超窗则 K 递减再压一轮、压缩调用≤2、K=0 仍超按原超限错误返回（2.3/8.3）。
func TestAutoCompactStillTooLargeReturnsOriginalError(t *testing.T) {
	var origCalls, compCalls int32
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&origCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&compCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-sum","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"summarized"},"finish_reason":"stop"}]}`))
	}))
	defer comp.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL), compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model:    "orig",
		Messages: autoMsgs(),
	}, "client-key")

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "maximum context length") {
		t.Fatalf("status=%d body=%s, want original 400", w.Code, w.Body.String())
	}
	if compCalls != 2 {
		t.Fatalf("compress calls=%d, want 2 (cap)", compCalls)
	}
	// orig 至少 3 次 dispatch（原始 + 两轮续命），且不应无限重试
	if origCalls > 3 {
		t.Fatalf("orig calls=%d, want <=3 (bounded)", origCalls)
	}
}

// 未配置 compress_model：超限直接透传，零回归（2.3/5.3）。
func TestAutoCompactNotTriggeredWithoutCompressModel(t *testing.T) {
	var origCalls int32
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&origCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()

	// origLease 不带 compress_model（未配置：零回归）
	r := &plainReplay{lease: leaseFor("orig-1", orig.URL)}
	f := NewForwarder(&config.Config{}, r, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model:    "orig",
		Messages: autoMsgs(),
	}, "client-key")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", w.Code)
	}
	if origCalls != 1 || len(r.dispatches) != 1 {
		t.Fatalf("origCalls=%d dispatches=%d, want 1/1", origCalls, len(r.dispatches))
	}
}

// plainReplay 固定返回单一 lease（不带 compress_model）。
type plainReplay struct {
	lease      replayv1.TargetLease
	dispatches []replayv1.DispatchRequest
}

func (r *plainReplay) Dispatch(_ context.Context, d replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	r.dispatches = append(r.dispatches, d)
	return r.lease, nil
}

func (*plainReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{}, nil
}

// 单条消息超窗（2.5/8.4）：无旧历史可压缩，不触发压缩直接返回。
func TestAutoCompactNoOldHistory(t *testing.T) {
	var origCalls int32
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&origCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`))
	}))
	defer orig.Close()
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("compress model must not be called when there is no old history")
	}))
	defer comp.Close()

	replay := &compactReplay{origModel: "orig", origLease: leaseFor("orig-1", orig.URL), compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model:    "orig",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "one huge message"}}}},
	}, "client-key")

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "maximum context length") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if origCalls != 1 || len(replay.dispatched()) != 1 {
		t.Fatalf("origCalls=%d dispatches=%d, want 1/1", origCalls, len(replay.dispatched()))
	}
}

// 调度层窗口过滤 flavor（8.1 dispatch 路径）：orig dispatch 直接 413
// context_too_large → 第二档压缩 → 续命成功。
func TestAutoCompactDispatchTooLargeFlavor(t *testing.T) {
	comp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-sum","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"summarized"},"finish_reason":"stop"}]}`))
	}))
	defer comp.Close()
	var finalCalls int32
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&finalCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","model":"native","choices":[{"index":0,"message":{"role":"assistant","content":"after-compact"},"finish_reason":"stop"}]}`))
	}))
	defer orig.Close()

	// orig 模型首次 dispatch 返回 context_too_large（窗口过滤），
	// 带 CompressOf 的 dispatch 成功返回 orig lease（续命用）
	r := &windowFilterReplay{origLease: leaseFor("orig-1", orig.URL), compLease: leaseFor("comp-1", comp.URL)}
	f := NewForwarder(&config.Config{}, r, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model:    "orig",
		Messages: autoMsgs(),
	}, "client-key")

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "after-compact") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-ModelSurge-Compacted"); got != "true" {
		t.Fatalf("X-ModelSurge-Compacted=%q, want true", got)
	}
	if finalCalls != 1 {
		t.Fatalf("final orig calls=%d, want 1", finalCalls)
	}
}

// windowFilterReplay：orig 首次 dispatch 返回 context_too_large（窗口过滤，
// 信封带 compress_model），续命 redispatch 返回 lease；带 CompressOf 的
// dispatch 分流到压缩 lease。
type windowFilterReplay struct {
	origLease  replayv1.TargetLease
	compLease  replayv1.TargetLease
	origSeen   int32
	dispatches []replayv1.DispatchRequest
}

func (r *windowFilterReplay) Dispatch(_ context.Context, d replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	r.dispatches = append(r.dispatches, d)
	if d.CompressOf == "orig" && d.Model == "comp" {
		return r.compLease, nil
	}
	if d.Model == "orig" {
		if atomic.AddInt32(&r.origSeen, 1) == 1 {
			return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeContextTooLarge, Message: "estimated 300000 tokens exceed all candidate context windows", CompressModel: "comp"}
		}
		return r.origLease, nil
	}
	return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeNotFound, Message: "user model not found"}
}

func (*windowFilterReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{}, nil
}
