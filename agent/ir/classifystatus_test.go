package ir

import "testing"

// 402 与 413 不得落到 default 的 upstream_error。402 是账号余额/配额耗尽，属账号级
// 问题（换号可救，与 403/429 同列）；413 是请求体过大，换谁都会被同样拒绝，与 relay
// 自己造 413 的两处（contexterr.go、replayDispatchError 的 CodeContextTooLarge）同为
// invalid_request。判成 upstream_error 会让客户端读成「服务端故障」并按 5xx 语义
// 反复重试一个必然失败的请求。
func TestClassifyStatusQuotaAndTooLarge(t *testing.T) {
	cases := []struct {
		status int
		typ    string
		retry  bool
	}{
		{402, ErrTypeRateLimit, true},
		{413, ErrTypeInvalidReq, false},
		// 408/425 与 504/524 同为连接级瞬时故障，必须可重试：修复前落 default 判
		// 不可重试，实测第一个目标就放弃（dispatch=1），而 504 换满整池（dispatch=6）。
		{408, ErrTypeOverloaded, true},
		{425, ErrTypeOverloaded, true},
		// 既有档位一并钉住，防止补码时顺带改错邻居
		{400, ErrTypeInvalidReq, false},
		{401, ErrTypeAuth, true},
		{403, ErrTypePermission, true},
		{404, ErrTypeNotFound, false},
		{429, ErrTypeRateLimit, true},
		{500, ErrTypeOverloaded, true},
		{503, ErrTypeOverloaded, true},
		{504, ErrTypeOverloaded, true},
	}
	for _, c := range cases {
		typ, retry := ClassifyStatus(c.status)
		if typ != c.typ || retry != c.retry {
			t.Errorf("ClassifyStatus(%d) = (%q, %v)，want (%q, %v)", c.status, typ, retry, c.typ, c.retry)
		}
	}
}

// errors.go 声明的不变量：ClassifyStatus **覆盖到**的状态码，其可重试性必须与
// StreamRetryable(它产出的类型) 一致，否则同一个错误在流式与非流式两条路径上得出
// 相反的重试结论（R83 之后聚合路径正是按 StreamRetryable 重算可重试性的）。
func TestClassifyStatusCoveredCodesAgreeWithStreamRetryable(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 408, 413, 425, 429, 500, 502, 503, 504} {
		typ, retry := ClassifyStatus(status)
		if got := StreamRetryable(typ); got != retry {
			t.Errorf("status=%d type=%q：ClassifyStatus 判 retryable=%v，StreamRetryable 判 %v（两条路径口径相反）",
				status, typ, retry, got)
		}
	}
}

// 未显式覆盖的状态码留在 default：upstream_error + 不可重试。拆分之后它与
// StreamRetryable(upstream_error)=false 同口径——传输/解码失败已改归
// connection_error（可重试），upstream_error 只剩未映射状态码一个来源。
// 钉住两侧一致：将来改 default 必须是有意的，并同步 StreamRetryable。
func TestClassifyStatusUncoveredCodesStayNonRetryable(t *testing.T) {
	for _, status := range []int{405, 409, 410, 418, 422, 451} {
		typ, retry := ClassifyStatus(status)
		if typ != ErrTypeUpstream || retry {
			t.Errorf("ClassifyStatus(%d) = (%q, %v)，want (%q, false)", status, typ, retry, ErrTypeUpstream)
		}
		if StreamRetryable(typ) {
			t.Errorf("status=%d：ClassifyStatus 判不可重试但 StreamRetryable(%q)=true（两条路径口径相反）", status, typ)
		}
	}
}

// 超时家族必须整体同调：408（请求超时）、425（0-RTT 重放被拒）、504/524（网关
// 超时）都是「换一条连接就可能成」的瞬时故障。修复前只有 >=500 那一半可重试，
// 同一种故障因状态码差一位拿到相反的调度动作。
//
// 这条是不变量测试（同 R84 的跨路径口径测试）：将来再往 default 里掉一个超时码，
// 或把 4xx 超时单独判死，都会在这里炸，而不是等到线上池子只用了一个账号才发现。
func TestClassifyStatusTimeoutFamilyIsUniformlyRetryable(t *testing.T) {
	for _, status := range []int{408, 425, 504, 524, 598, 599} {
		typ, retry := ClassifyStatus(status)
		if !retry {
			t.Errorf("ClassifyStatus(%d) retryable=false，超时家族必须可重试（类型 %q）", status, typ)
		}
		if typ != ErrTypeOverloaded {
			t.Errorf("ClassifyStatus(%d) type=%q，want %q（同族必须同规范类型，否则客户端退避策略分叉）",
				status, typ, ErrTypeOverloaded)
		}
		if got := StreamRetryable(typ); !got {
			t.Errorf("status=%d：ClassifyStatus 判可重试但 StreamRetryable(%q)=false，两条路径口径相反", status, typ)
		}
	}
}

// 显式状态码优先于类型反推：402 归到 rate_limit_error 之后，对客户端仍须是 402，
// 不能被 HTTPStatus() 改写成 429。
func TestPaymentRequiredKeepsItsOwnStatus(t *testing.T) {
	e := NewHTTPError(402, "credit balance exhausted")
	if e.StatusCode != 402 || e.HTTPStatus() != 402 {
		t.Errorf("StatusCode=%d HTTPStatus=%d，want 402/402（类型反推不得覆盖真实码）", e.StatusCode, e.HTTPStatus())
	}
	if !e.Retryable {
		t.Error("402 必须可重试：换一个账号才有救")
	}
	if e := NewHTTPError(413, "payload too large"); e.HTTPStatus() != 413 || e.Retryable {
		t.Errorf("413 -> status=%d retryable=%v，want 413/false", e.HTTPStatus(), e.Retryable)
	}
}
