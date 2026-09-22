package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// kiroerror_test.go kiro 远程上游错误的规范外形保真。
//
// kiro 的数据面在 Upstream 侧，agent 通过 replay 的 /kiro/execute 拿到一个
// 透传的 *http.Response。错误体有两种外形：ModelSurge 的信封
// {"error":{code,message,retryable,status,reason}}，以及非 ModelSurge 的东西
// （代理插的 502、HTML 错误页、空 body）。此前两条都把规范类型写死成
// upstream_error，而普通协议路径走 ir.NewHTTPError 按状态码推断——同一个上游
// 429，走 anthropic 直连的客户端收到 rate_limit_error、走 kiro 目标的收到
// upstream_error。

// kiroErrReplay 让 ExecuteKiro 返回指定的状态码与错误体。
type kiroErrReplay struct {
	status int
	body   string
}

func (*kiroErrReplay) Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	return replayv1.TargetLease{RequestID: "req", GroupID: "g", TargetID: "kiro-1", Protocol: "kiro"}, nil
}

func (*kiroErrReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{Applied: true}, nil
}

func (*kiroErrReplay) WebSearch(context.Context, replayv1.WebSearchRequest) (replayv1.WebSearchResponse, error) {
	return replayv1.WebSearchResponse{}, nil
}

func (r *kiroErrReplay) ExecuteKiro(context.Context, replayv1.KiroExecuteRequest) (*http.Response, error) {
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(r.body))),
	}, nil
}

func kiroOpenErr(t *testing.T, status int, body string) *ir.Error {
	t.Helper()
	f := NewForwarder(&config.Config{}, &kiroErrReplay{status: status, body: body}, nil)
	cand, err := resolvedCandidate(replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: "kiro-1", Protocol: "kiro",
	})
	if err != nil {
		t.Fatalf("resolvedCandidate: %v", err)
	}
	resp, irErr := f.openKiroReplay(t.Context(), cand, json.RawMessage(`{}`), "")
	if resp != nil {
		t.Errorf("错误响应不得把 *http.Response 交回调用方（会漏关 body）")
	}
	if irErr == nil {
		t.Fatalf("status=%d 未产出 ir.Error", status)
	}
	return irErr
}

func envelope(code, message string, retryable bool, status int, reason string) string {
	b, err := json.Marshal(replayv1.ErrorEnvelope{Error: replayv1.Error{
		Code: code, Message: message, Retryable: retryable, Status: status, Reason: reason,
	}})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// 规范类型必须按状态码推断，与普通协议路径（ir.NewHTTPError）同一条规则。
// 写死 upstream_error 会让按 type 判断是否退避重试的 SDK 得到相反结论。
func TestKiroUpstreamErrorUsesCanonicalType(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{400, ir.ErrTypeInvalidReq},
		{401, ir.ErrTypeAuth},
		{403, ir.ErrTypePermission},
		{404, ir.ErrTypeNotFound},
		{429, ir.ErrTypeRateLimit},
		{500, ir.ErrTypeOverloaded},
		{503, ir.ErrTypeOverloaded},
	}
	for _, c := range cases {
		e := kiroOpenErr(t, c.status, envelope("target_unavailable", "boom", true, c.status, ""))
		if e.Type != c.want {
			t.Errorf("status=%d type=%q，want %q", c.status, e.Type, c.want)
		}
		if e.StatusCode != c.status {
			t.Errorf("status=%d 实得 StatusCode=%d", c.status, e.StatusCode)
		}
		if e.Message != "boom" {
			t.Errorf("status=%d 上游消息丢失：msg=%q", c.status, e.Message)
		}
	}
}

// 跨路径不变量：同一个状态码，kiro 路径与普通路径必须得出同一个规范类型。
// 这条断言直接锁住「两条路径口径漂移」，比逐个枚举类型更能防止将来再分叉。
func TestKiroAndNormalPathAgreeOnCanonicalType(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 409, 413, 422, 429, 500, 502, 503} {
		kiroErr := kiroOpenErr(t, status, envelope("target_unavailable", "same message", true, status, ""))
		normal := ir.NewHTTPError(status, "same message")
		if kiroErr.Type != normal.Type {
			t.Errorf("status=%d：kiro 路径 type=%q，普通路径 type=%q（口径漂移）",
				status, kiroErr.Type, normal.Type)
		}
	}
}

// 信封里的 code 不得丢：openai-chat / openai-responses 的 errorBody 有 code 槽，
// 丢了这个槽对客户端永远是空的。
func TestKiroUpstreamErrorCodePreserved(t *testing.T) {
	for _, code := range []string{
		replayv1.CodeTargetUnavailable, replayv1.CodeUnauthorized,
		replayv1.CodeInvalidRequest, replayv1.CodeContextTooLarge,
	} {
		e := kiroOpenErr(t, 400, envelope(code, "boom", false, 400, ""))
		if e.Code != code {
			t.Errorf("code=%q 未透传，实得 %q", code, e.Code)
		}
	}
}

// reason 是 kiro 调度决策的输入（INVALID_MODEL_ID -> 换目标），必须原样保留。
func TestKiroUpstreamErrorKeepsReason(t *testing.T) {
	e := kiroOpenErr(t, 400, envelope("invalid_request", "bad model", false, 400, "INVALID_MODEL_ID"))
	if e.Reason != "INVALID_MODEL_ID" {
		t.Fatalf("reason=%q，want INVALID_MODEL_ID", e.Reason)
	}
	if got := localResultAction(candidate{protocol: "kiro"}, e, 0, 3); got != replayv1.ActionSwitchTarget {
		t.Errorf("INVALID_MODEL_ID 应换目标，实得 %s", got)
	}
}

// 可重试性取信封的值，不取 ClassifyStatus 的那一半：Upstream 的
// ClassifyKiroError 认得 kiro 原因码，单看状态码推不出来。这条锁住
// 「只借类型、不借可重试性」这个刻意的不对称，防止将来被"顺手统一"掉。
func TestKiroUpstreamErrorKeepsEnvelopeRetryable(t *testing.T) {
	// 400 按状态码不可重试，但 INVALID_MODEL_ID 换一个账号就成了。
	e := kiroOpenErr(t, 400, envelope("invalid_request", "bad model", true, 400, "INVALID_MODEL_ID"))
	if !e.Retryable {
		t.Error("信封说可重试就必须可重试，不得被状态码推断覆盖")
	}
	// 反向：500 按状态码可重试，但信封判定为致命时不得强行重试。
	e = kiroOpenErr(t, 500, envelope("internal_error", "fatal", false, 500, "CONTENT_LENGTH_EXCEEDS_THRESHOLD"))
	if e.Retryable {
		t.Error("信封说不可重试就必须不可重试")
	}
	if got := localResultAction(candidate{protocol: "kiro"}, e, 0, 3); got != replayv1.ActionStop {
		t.Errorf("致命错误应停止，实得 %s", got)
	}
}

// 信封的 status 是真实上游状态码，比传输层状态码权威；缺席时才回落传输层。
func TestKiroEnvelopeStatusOverridesTransportStatus(t *testing.T) {
	e := kiroOpenErr(t, 502, envelope("target_unavailable", "throttled", true, 429, "THROTTLING"))
	if e.StatusCode != 429 {
		t.Fatalf("StatusCode=%d，want 429（信封优先）", e.StatusCode)
	}
	if e.Type != ir.ErrTypeRateLimit {
		t.Errorf("type=%q，want %q（类型要按信封的状态码推，不是传输层的 502）", e.Type, ir.ErrTypeRateLimit)
	}
}

func TestKiroEnvelopeStatusZeroFallsBackToTransport(t *testing.T) {
	e := kiroOpenErr(t, 429, envelope("target_unavailable", "throttled", true, 0, ""))
	if e.StatusCode != 429 {
		t.Fatalf("StatusCode=%d，want 429（回落传输层）", e.StatusCode)
	}
	if e.Type != ir.ErrTypeRateLimit {
		t.Errorf("type=%q，want %q", e.Type, ir.ErrTypeRateLimit)
	}
}

// 非信封 body（代理插的 502 / HTML 错误页 / 空 body）同样要按状态码分类，
// 与普通路径完全一致。此前用 status >= 500 判可重试：401 被判成不可重试，
// 于是同一个 401 仅仅因为有没有信封，在 attempt > 0 时一个换目标重试、
// 一个直接放弃。
func TestKiroBareErrorBodyClassifiedLikeNormalPath(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 429, 500, 503} {
		e := kiroOpenErr(t, status, `{"message":"slow down"}`)
		normal := ir.NewHTTPError(status, "ignored")
		if e.Type != normal.Type {
			t.Errorf("status=%d 裸 body：type=%q，普通路径 %q", status, e.Type, normal.Type)
		}
		if e.Retryable != normal.Retryable {
			t.Errorf("status=%d 裸 body：retryable=%v，普通路径 %v", status, e.Retryable, normal.Retryable)
		}
	}
}

// 回归锁：401 有没有信封都必须可重试，且 attempt > 0 时决策一致。
// 这是修复前唯一能被观测到的调度分歧。
func TestKiro401RetryableRegardlessOfEnvelope(t *testing.T) {
	bare := kiroOpenErr(t, 401, `{"message":"token expired"}`)
	wrapped := kiroOpenErr(t, 401, envelope("unauthorized", "token expired", true, 401, ""))
	for _, e := range []*ir.Error{bare, wrapped} {
		if !e.Retryable {
			t.Errorf("401 必须可重试（换账号可能就成了）：msg=%q", e.Message)
		}
		if e.Type != ir.ErrTypeAuth {
			t.Errorf("401 type=%q，want %q", e.Type, ir.ErrTypeAuth)
		}
	}
	cand := candidate{protocol: "kiro"}
	if a, b := localResultAction(cand, bare, 2, 3), localResultAction(cand, wrapped, 2, 3); a != b {
		t.Errorf("attempt=2 时决策分歧：裸 body=%s 信封=%s", a, b)
	}
}

// 客户端可见面：anthropic 入口只渲染 type 与 message，所以 type 错了客户端
// 就完全看不到真实原因类别。
func TestKiroErrorRenderedToClientCarriesCanonicalType(t *testing.T) {
	anth := proto.MustInbound("anthropic")
	e := kiroOpenErr(t, 429, envelope("target_unavailable", "upstream throttled", true, 429, "THROTTLING"))
	status, body := anth.RenderError(e)
	if status != 429 {
		t.Errorf("客户端状态码=%d，want 429", status)
	}
	if !strings.Contains(string(body), `"type":"rate_limit_error"`) {
		t.Errorf("客户端 body 缺规范类型：%s", body)
	}
	if strings.Contains(string(body), "upstream_error") {
		t.Errorf("客户端 body 仍含 upstream_error：%s", body)
	}
}

// openai 两系的 errorBody 有 code 槽，信封的 code 要一路走到客户端。
func TestKiroErrorCodeRenderedToOpenAIClient(t *testing.T) {
	e := kiroOpenErr(t, 429, envelope("target_unavailable", "upstream throttled", true, 429, ""))
	for _, name := range []string{"openai-chat", "openai-responses"} {
		_, body := proto.MustInbound(name).RenderError(e)
		if !strings.Contains(string(body), `"code":"target_unavailable"`) {
			t.Errorf("%s 客户端 body 缺 code：%s", name, body)
		}
		if !strings.Contains(string(body), `"type":"rate_limit_error"`) {
			t.Errorf("%s 客户端 body 缺规范类型：%s", name, body)
		}
	}
}

// 成功路径不受影响：2xx 必须把响应原样交回，不产出错误。
func TestKiroSuccessPathUnaffected(t *testing.T) {
	f := NewForwarder(&config.Config{}, &kiroErrReplay{status: 200, body: `{"ok":true}`}, nil)
	cand, err := resolvedCandidate(replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: "kiro-1", Protocol: "kiro",
	})
	if err != nil {
		t.Fatalf("resolvedCandidate: %v", err)
	}
	resp, irErr := f.openKiroReplay(t.Context(), cand, json.RawMessage(`{}`), "")
	if irErr != nil {
		t.Fatalf("2xx 不应产出错误：%v", irErr)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatal("2xx 必须把响应交回调用方")
	}
	resp.Body.Close()
}
