package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// 调度动作只由错误本身决定，与目标协议无关：localResultAction 的签名里已经没有
// 候选，这条矩阵钉住「状态码 → 动作」这张表。402 曾在 default 分支被判成不可重试
// 直接 stop，池子里明明还有可用账号却只用了一个。
func TestLocalResultActionByStatus(t *testing.T) {
	const sameTargetRetries = 3
	cases := []struct {
		status        int
		wantFirst     string // attempt=0
		wantExhausted string // attempt=sameTargetRetries
	}{
		{400, replayv1.ActionStop, replayv1.ActionStop},
		{401, replayv1.ActionRetryTarget, replayv1.ActionSwitchTarget},
		// 402/429 走专属分支：同目标重试耗尽与否都直接换号。
		{402, replayv1.ActionSwitchTarget, replayv1.ActionSwitchTarget},
		{403, replayv1.ActionRetryTarget, replayv1.ActionSwitchTarget},
		{404, replayv1.ActionStop, replayv1.ActionStop},
		{405, replayv1.ActionStop, replayv1.ActionStop},
		{408, replayv1.ActionRetryTarget, replayv1.ActionSwitchTarget},
		{409, replayv1.ActionStop, replayv1.ActionStop},
		{410, replayv1.ActionStop, replayv1.ActionStop},
		{413, replayv1.ActionStop, replayv1.ActionStop},
		{422, replayv1.ActionStop, replayv1.ActionStop},
		{425, replayv1.ActionRetryTarget, replayv1.ActionSwitchTarget},
		{429, replayv1.ActionSwitchTarget, replayv1.ActionSwitchTarget},
		{451, replayv1.ActionStop, replayv1.ActionStop},
		{500, replayv1.ActionRetryTarget, replayv1.ActionSwitchTarget},
		{502, replayv1.ActionRetryTarget, replayv1.ActionSwitchTarget},
		{503, replayv1.ActionRetryTarget, replayv1.ActionSwitchTarget},
		{504, replayv1.ActionRetryTarget, replayv1.ActionSwitchTarget},
	}
	for _, c := range cases {
		e := ir.NewHTTPError(c.status, "boom")
		t.Run(fmt.Sprintf("%d/first", c.status), func(t *testing.T) {
			if got := localResultAction(e, 0, sameTargetRetries); got != c.wantFirst {
				t.Errorf("status=%d attempt=0 action=%q，want %q", c.status, got, c.wantFirst)
			}
		})
		t.Run(fmt.Sprintf("%d/exhausted", c.status), func(t *testing.T) {
			if got := localResultAction(e, sameTargetRetries, sameTargetRetries); got != c.wantExhausted {
				t.Errorf("status=%d attempt=%d action=%q，want %q", c.status, sameTargetRetries, got, c.wantExhausted)
			}
		})
	}
}

// 上下文超限一律 stop：换账号、原地重试都不会让请求变小，重试只是把同一个必然
// 失败的请求再发几遍。这条优先于状态码——上游可能报成 400 也可能报成 429。
func TestLocalResultActionStopsOnContextExceeded(t *testing.T) {
	for _, status := range []int{400, 429, 500} {
		e := ir.NewHTTPError(status, "boom")
		e.Reason = ReasonContextExceeded
		if got := localResultAction(e, 0, 3); got != replayv1.ActionStop {
			t.Errorf("status=%d 上下文超限 action=%q，want stop", status, got)
		}
	}
}

// nil 错误不是失败，但也不该让调度器继续换目标。
func TestLocalResultActionNilErrorStops(t *testing.T) {
	if got := localResultAction(nil, 0, 3); got != replayv1.ActionStop {
		t.Errorf("nil error action=%q，want stop", got)
	}
}

// 端到端：上游返回 402（账号欠费）时必须换着账号试完整个池子。
// 修复前 dispatch=1 —— 池里 4 个可用账号一个都没用上。
func TestPaymentRequiredBurnsPool(t *testing.T) {
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
