package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// 跨路径不变量：同一个上游状态码，走 kiro 目标与走普通协议目标必须得出同一个调度
// 动作。402 曾在此分叉——kiro 专属分支换目标，普通协议被 ClassifyStatus 的 default
// 判成不可重试直接 stop，池子里明明还有可用账号却只用了一个。
func TestLocalResultActionAgreesAcrossProtocols(t *testing.T) {
	statuses := []int{400, 401, 402, 403, 404, 405, 408, 409, 410, 413, 422, 429, 451, 500, 502, 503, 504}
	for _, status := range statuses {
		e := ir.NewHTTPError(status, "boom")
		for _, attempt := range []int{0, 2} {
			kiro := localResultAction(candidate{protocol: "kiro"}, e, attempt, 3)
			norm := localResultAction(candidate{protocol: "anthropic"}, e, attempt, 3)
			if kiro != norm {
				t.Errorf("status=%d attempt=%d：kiro=%q 普通协议=%q（跨路径口径相反）", status, attempt, kiro, norm)
			}
		}
	}
}

// 402/429 都要**立刻**换目标，不在同一个账号上重试：账号没钱不会因为再问一次就有钱，
// 限流也不会。同目标重试只是白烧一轮配额窗口。
func TestLocalResultActionSwitchesImmediatelyOnQuota(t *testing.T) {
	for _, status := range []int{http.StatusPaymentRequired, http.StatusTooManyRequests} {
		e := ir.NewHTTPError(status, "boom")
		if got := localResultAction(candidate{protocol: "anthropic"}, e, 0, 3); got != replayv1.ActionSwitchTarget {
			t.Errorf("status=%d attempt=0 action=%q，want switch_target", status, got)
		}
		if got := localResultAction(candidate{protocol: "kiro"}, e, 0, 3); got != replayv1.ActionSwitchTarget {
			t.Errorf("kiro status=%d attempt=0 action=%q，want switch_target", status, got)
		}
	}
}

// kiro 的 401/403 首次尝试仍原地重试：kiro 的 token 刷新有竞态，Upstream 会把 401
// 判成不可重试（ClassifyKiroError 的 recoverable 集里没有 401），这条 kiro 专属救援
// 不得被 402 的通用化改动顺带抹掉。
func TestKiroAuthFailureStillRetriesTargetOnce(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		e := &ir.Error{StatusCode: status, Type: ir.ErrTypeAuth, Message: "boom", Retryable: false}
		if got := localResultAction(candidate{protocol: "kiro"}, e, 0, 3); got != replayv1.ActionRetryTarget {
			t.Errorf("kiro status=%d attempt=0 action=%q，want retry_target", status, got)
		}
		if got := localResultAction(candidate{protocol: "kiro"}, e, 2, 3); got != replayv1.ActionStop {
			t.Errorf("kiro status=%d attempt=2 action=%q，want stop（上游已判不可重试）", status, got)
		}
	}
}

// 端到端：普通协议目标返回 402（账号欠费）时必须换着账号试完整个池子。
// 修复前 dispatch=1 —— 池里 4 个可用账号一个都没用上。
func TestPaymentRequiredBurnsPoolOnNormalProtocol(t *testing.T) {
	var hits atomic.Int32
	up := statusUpstream(t, http.StatusPaymentRequired, "credit balance exhausted", &hits)
	defer up.Close()
	rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
	w := poolForward(t, rp, "m", false)
	if rp.calls != poolLimit+1 || hits.Load() != poolLimit {
		t.Fatalf("402 应换满一池：dispatch=%d upstreamHits=%d，want %d/%d", rp.calls, hits.Load(), poolLimit+1, poolLimit)
	}
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("客户端状态码=%d，want 402（不得被类型反推改写成 429）", w.Code)
	}
	if !strings.Contains(w.Body.String(), ir.ErrTypeRateLimit) {
		t.Fatalf("客户端错误类型=%s，want 含 %s", w.Body.String(), ir.ErrTypeRateLimit)
	}
}

// kiro 路径的 402 行为不得因专属分支被通用分支接管而变化：仍然换满一池，
// 且规范类型与普通路径一致（rate_limit_error，不再是 upstream_error）。
func TestPaymentRequiredBurnsPoolOnKiro(t *testing.T) {
	rp := &quotaKiroReplay{status: http.StatusPaymentRequired, limit: poolLimit}
	w := quotaForward(t, rp, "public")
	if rp.calls != poolLimit+1 {
		t.Fatalf("kiro 402 dispatch=%d，want %d", rp.calls, poolLimit+1)
	}
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("客户端状态码=%d，want 402", w.Code)
	}
	if !strings.Contains(w.Body.String(), ir.ErrTypeRateLimit) {
		t.Fatalf("客户端错误类型=%s，want 含 %s", w.Body.String(), ir.ErrTypeRateLimit)
	}
}

// 413（请求体过大）不可重试：换账号也会被同样拒绝，烧池子毫无意义。客户端拿到的
// 规范类型必须是 invalid_request_error —— 与 relay 自己造 413 的两处一致，而不是
// upstream_error（客户端会读成服务端故障并按 5xx 语义反复重试）。
// 上游消息刻意不含上下文超限特征，避开 classifyContextError 的那条路。
func TestRequestEntityTooLargeStopsWithInvalidRequestType(t *testing.T) {
	var hits atomic.Int32
	up := statusUpstream(t, http.StatusRequestEntityTooLarge, "payload rejected", &hits)
	defer up.Close()
	rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
	w := poolForward(t, rp, "m", false)
	if rp.calls != 1 || hits.Load() != 1 {
		t.Fatalf("413 不可重试：dispatch=%d upstreamHits=%d，want 1/1", rp.calls, hits.Load())
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("客户端状态码=%d，want 413", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, ir.ErrTypeInvalidReq) || strings.Contains(body, ir.ErrTypeUpstream) {
		t.Fatalf("客户端错误类型=%s，want 含 %s 且不含 %s", body, ir.ErrTypeInvalidReq, ir.ErrTypeUpstream)
	}
}

func statusUpstream(t *testing.T, status int, message string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"type":"x","message":` + fmt.Sprintf("%q", message) + `}}`))
	}))
}

func quotaForward(t *testing.T, rp Replay, model string) *httptest.ResponseRecorder {
	t.Helper()
	f := NewForwarder(&config.Config{}, rp, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: model, MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	return w
}

// quotaKiroReplay 让 ExecuteKiro 返回给定状态码与空 body（非 ModelSurge 信封形态，
// 走 openKiroReplay 的 ir.NewHTTPError 分支）。每次 Dispatch 发一个新 TargetID，
// 超过 limit 就报池子空了，好把「烧了几个账号」变成可数的。
type quotaKiroReplay struct {
	status int
	limit  int
	calls  int
}

func (r *quotaKiroReplay) Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	r.calls++
	if r.calls > r.limit {
		return replayv1.TargetLease{}, fmt.Errorf("pool exhausted after %d targets", r.limit)
	}
	return replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: fmt.Sprintf("t%d", r.calls),
		Protocol: "kiro", NativeModel: "public",
	}, nil
}

func (*quotaKiroReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{Applied: true}, nil
}

func (*quotaKiroReplay) WebSearch(context.Context, replayv1.WebSearchRequest) (replayv1.WebSearchResponse, error) {
	return replayv1.WebSearchResponse{}, nil
}

func (r *quotaKiroReplay) ExecuteKiro(context.Context, replayv1.KiroExecuteRequest) (*http.Response, error) {
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       http.NoBody,
	}, nil
}
