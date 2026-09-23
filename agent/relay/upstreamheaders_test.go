// upstreamheaders_test.go 上游成功响应的头回传保真。
//
// 两条不变量：白名单内的上游头（厂商限流族 + 上游 request id）必须抵达客户端，
// 无论走哪条写出路径；白名单外的头（framing 头、厂商内部头、Set-Cookie）一个
// 都不得泄漏——我们的 body 是重新编码过的，照抄上游 Content-Length 会让客户端
// 按错误长度截断或挂死。
//
// 断言一律走 w.Result().Header 而不是 w.Header()：ResponseRecorder 在
// WriteHeader 那一刻给头做快照，之后往 HeaderMap 里塞的值只会出现在 w.Header()
// 里、不会出现在真正交给客户端的 Result() 里。用 w.Header() 断言的话，「头设晚了」
// 这个唯一的真实失效形态测不出来。
package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
)

// wireHeader 客户端真正收到的头（写出头之后的修改不计）。
func wireHeader(w *httptest.ResponseRecorder) http.Header { return w.Result().Header }

// headerUpstream 伪造一个 anthropic 上游，附带指定的响应头。sse=false 时回完整
// JSON（触发「上游忽略 stream=true」的兜底聚合路径）。
func headerUpstream(t *testing.T, h http.Header, sse bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, vs := range h {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, f := range []string{
				"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"m1","model":"claude","usage":{"input_tokens":100}}}`,
				"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
				"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}`,
				"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
				"event: message_stop\ndata: " + `{"type":"message_stop"}`,
			} {
				_, _ = w.Write([]byte(f + "\n\n"))
				w.(http.Flusher).Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func vendorHeaders() http.Header {
	return http.Header{
		"X-Request-Id":                           []string{"req_up_1"},
		"X-Ratelimit-Limit-Requests":             []string{"100"},
		"X-Ratelimit-Remaining-Requests":         []string{"3"},
		"Anthropic-Ratelimit-Requests-Remaining": []string{"7"},
	}
}

// 限流头是客户端做自适应退避与配额展示的唯一信号；全丢的话客户端只能当作
// 「无限制」，然后在 429 上硬撞。三条写出路径都要覆盖。
func TestUpstreamRateLimitHeadersReachClient(t *testing.T) {
	for _, sse := range []bool{true, false} {
		for _, client := range []string{"anthropic", "openai-chat"} {
			t.Run("sse="+boolStr(sse)+"/client="+client, func(t *testing.T) {
				up := headerUpstream(t, vendorHeaders(), sse)
				rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
				w := forwardUsage(t, rp, client, &config.Config{}, nil)
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				h := wireHeader(w)
				for key, want := range map[string]string{
					"X-Request-Id":                           "req_up_1",
					"X-Ratelimit-Limit-Requests":             "100",
					"X-Ratelimit-Remaining-Requests":         "3",
					"Anthropic-Ratelimit-Requests-Remaining": "7",
				} {
					if got := h.Get(key); got != want {
						t.Errorf("%s=%q, want %q（实际收到的头：%v）", key, got, want, h)
					}
				}
			})
		}
	}
}

// framing 头与厂商内部头一律不得复制：我们的 body 是重新编码过的，长度与上游
// 无关；内部头（账号标识、会话 cookie）泄漏给客户端是越权披露。
func TestUpstreamFramingAndInternalHeadersNotCopied(t *testing.T) {
	resp := &http.Response{Header: http.Header{
		"Content-Length":     []string{"5"},
		"Transfer-Encoding":  []string{"chunked"},
		"Connection":         []string{"close"},
		"Set-Cookie":         []string{"session=abc"},
		"X-Internal-Account": []string{"acct-secret"},
		"X-Request-Id":       []string{"req_up_1"},
	}}
	w := httptest.NewRecorder()
	forwardUpstreamHeaders(w, resp)
	if got := w.Header().Get("X-Request-Id"); got != "req_up_1" {
		t.Errorf("白名单头没过去：%q", got)
	}
	// Connection 由本仓按 SSE 需要自己设置，不得被上游的 close 覆盖。
	for _, banned := range []string{"Content-Length", "Transfer-Encoding", "Set-Cookie", "X-Internal-Account", "Connection"} {
		if _, ok := w.Header()[banned]; ok {
			t.Errorf("%s 被复制给了客户端：%v", banned, w.Header()[banned])
		}
	}
}

// 同名多值整族带走（厂商会重复发同一个头），且换账号重试时用最后一次成功的
// 上游值替换、不叠加成两个值。
func TestForwardUpstreamHeadersReplacesAcrossAttempts(t *testing.T) {
	w := httptest.NewRecorder()
	forwardUpstreamHeaders(w, &http.Response{Header: http.Header{
		"X-Ratelimit-Remaining-Tokens": []string{"900", "800"},
		"X-Request-Id":                 []string{"req_first"},
	}})
	forwardUpstreamHeaders(w, &http.Response{Header: http.Header{
		"X-Request-Id": []string{"req_second"},
	}})
	if got := w.Header().Values("X-Ratelimit-Remaining-Tokens"); len(got) != 2 || got[0] != "900" || got[1] != "800" {
		t.Errorf("多值没整族带走：%v", got)
	}
	if got := w.Header().Values("X-Request-Id"); len(got) != 1 || got[0] != "req_second" {
		t.Errorf("第二次尝试没替换掉第一次：%v", got)
	}
}

// 反向代理（nginx 一类）默认缓冲响应，SSE 会被攒到最后一次性吐出，客户端看到的
// 是「流式变非流式」。两个 SSE 出口都要显式关掉缓冲。
func TestSSEResponsesDisableProxyBuffering(t *testing.T) {
	t.Run("普通流", func(t *testing.T) {
		up := headerUpstream(t, nil, true)
		rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
		assertNoBuffering(t, forwardUsage(t, rp, "anthropic", &config.Config{}, nil))
	})
	t.Run("聚合转流", func(t *testing.T) {
		up := headerUpstream(t, nil, false) // 上游回 JSON，客户端要流式
		rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
		assertNoBuffering(t, forwardUsage(t, rp, "anthropic", &config.Config{}, nil))
	})
}

func assertNoBuffering(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := wireHeader(w).Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering=%q, want \"no\"（实际收到的头：%v）", got, wireHeader(w))
	}
}

// 非流式 JSON 响应同样要带头：客户端在同步调用上一样依赖限流信号，且这条路径
// 的 Content-Type 是我们自己写的，不能被上游的头带偏。
func TestNonStreamJSONResponseCarriesUpstreamHeaders(t *testing.T) {
	up := headerUpstream(t, vendorHeaders(), false)
	rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
	w := forwardUsage(t, rp, "anthropic", &config.Config{}, func(r *ir.Request) { r.Stream = false })
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	h := wireHeader(w)
	if got := h.Get("X-Ratelimit-Remaining-Requests"); got != "3" {
		t.Errorf("非流式缺限流头：%q（实际收到的头：%v）", got, h)
	}
	if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	if !strings.Contains(w.Body.String(), `"input_tokens":100`) {
		t.Errorf("正文里的 usage 丢了：%s", w.Body.String())
	}
}
