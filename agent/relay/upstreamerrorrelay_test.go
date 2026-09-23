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
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// bodyUpstream 返回一个每次都以给定状态码/响应头/响应体失败的上游。
func bodyUpstream(t *testing.T, status int, header http.Header, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// fwdTo 把一次请求以 client 协议打过去，返回客户端看到的响应。
func fwdTo(t *testing.T, rp Replay, client string) *httptest.ResponseRecorder {
	t.Helper()
	f := NewForwarder(&config.Config{}, rp, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound(client), &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	return w
}

// 上游错误体必须解析后再交给客户端。修复前 error.message 里装的是整段转义 JSON
// （`"{\"error\":{\"message\":\"Rate limit reached\",...}}"`）：用户界面显示的是一坨
// 转义字符串，SDK 想按 message 做关键词判断也只能匹配到 JSON 语法碎片。
func TestUpstreamErrorMessageReachesClientUnwrapped(t *testing.T) {
	const upstreamBody = `{"error":{"message":"Rate limit reached for gpt-4o on RPM","type":"requests","param":null,"code":"rate_limit_exceeded"}}`
	for _, client := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		t.Run(client, func(t *testing.T) {
			var hits atomic.Int32
			up := bodyUpstream(t, http.StatusTooManyRequests, nil, upstreamBody, &hits)
			defer up.Close()
			w := fwdTo(t, &poolReplay{baseURL: up.URL, limit: 1}, client)
			body := w.Body.String()
			if !strings.Contains(body, "Rate limit reached for gpt-4o on RPM") {
				t.Fatalf("客户端错误体里没有上游消息：%s", body)
			}
			// 转义引号是「原文整段入 Message」的特征；解析后不该再出现。
			if strings.Contains(body, `\"`) {
				t.Fatalf("客户端错误体里仍是转义后的原文 JSON：%s", body)
			}
			if strings.Contains(body, `"type":"requests"`) {
				t.Fatalf("上游私有 type 被当消息透传：%s", body)
			}
		})
	}
}

// 上游自报的错误码要落到客户端有槽位的三族（anthropic 官方错误体只有
// type + message，没有 code 位置——那是协议里就没有，不算丢失）。
func TestUpstreamErrorCodeReachesClientsThatHaveASlot(t *testing.T) {
	const upstreamBody = `{"error":{"message":"boom","type":"server_error","code":"context_length_exceeded"}}`
	for _, client := range []string{"openai-chat", "openai-responses", "gemini"} {
		t.Run(client, func(t *testing.T) {
			var hits atomic.Int32
			up := bodyUpstream(t, http.StatusBadRequest, nil, upstreamBody, &hits)
			defer up.Close()
			w := fwdTo(t, &poolReplay{baseURL: up.URL, limit: 1}, client)
			if !strings.Contains(w.Body.String(), "context_length_exceeded") {
				t.Fatalf("上游错误码丢失：%s", w.Body.String())
			}
		})
	}
}

// 上游在错误响应头里给的退避提示必须转给客户端：429 没有 Retry-After 时 SDK 只能
// 按自己的默认值瞎等，要么立刻重试撞第二次限流，要么等过头。new-api 与 sub2api
// 都在网关侧写这个头，ModelSurge 此前全链路不看上游响应头。
func TestRetryAfterIsForwardedToClient(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"seconds", "42"},
		{"seconds with space", "  7 "},
		{"http date", "Wed, 21 Oct 2026 07:28:00 GMT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			up := bodyUpstream(t, http.StatusTooManyRequests,
				http.Header{"Retry-After": []string{tc.value}}, `{"error":{"message":"slow down"}}`, &hits)
			defer up.Close()
			w := fwdTo(t, &poolReplay{baseURL: up.URL, limit: 1}, "anthropic")
			if got := w.Header().Get("Retry-After"); strings.TrimSpace(got) != strings.TrimSpace(tc.value) {
				t.Errorf("Retry-After = %q，want %q", got, tc.value)
			}
		})
	}
}

// 上游是外部边界，值不能照单全收：RFC 9110 只允许十进制秒数或 HTTP 日期。
// 别的形态一律丢掉——丢掉只是少个提示，透传垃圾等于把上游的话写进我们自己的响应头。
func TestRetryAfterRejectsMalformedUpstreamValue(t *testing.T) {
	for _, value := range []string{"soon", "-5", "42s", "0x2a", "4 2", "<script>x</script>"} {
		t.Run(value, func(t *testing.T) {
			var hits atomic.Int32
			up := bodyUpstream(t, http.StatusTooManyRequests,
				http.Header{"Retry-After": []string{value}}, `{"error":{"message":"slow down"}}`, &hits)
			defer up.Close()
			w := fwdTo(t, &poolReplay{baseURL: up.URL, limit: 1}, "anthropic")
			if got := w.Header().Get("Retry-After"); got != "" {
				t.Errorf("非法 Retry-After %q 被透传成 %q", value, got)
			}
		})
	}
	if got := sanitizeRetryAfter("42"); got != "42" {
		t.Errorf("合法秒数被误杀：%q", got)
	}
	if got := sanitizeRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT"); got == "" {
		t.Error("合法 HTTP 日期被误杀")
	}
}

// 上游没给就不得凭空造：成功响应与 relay 自造的本地错误都不该带这个头，
// 否则客户端会以为被限流而退避。
func TestNoRetryAfterInventedWithoutUpstreamHint(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(poolStart + "\n\n" + poolText + "\n\n" +
				`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
				`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
				`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"))
		}))
		defer up.Close()
		w := fwdTo(t, &poolReplay{baseURL: up.URL, limit: 1}, "anthropic")
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "" {
			t.Errorf("成功响应凭空带了 Retry-After=%q", got)
		}
	})
	t.Run("local error", func(t *testing.T) {
		// dispatch 直接失败：错误由 relay 自造，上游从未发过响应头
		f := NewForwarder(&config.Config{}, &failDispatchReplay{}, nil)
		w := httptest.NewRecorder()
		f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
			Model: "m", MaxTokens: 64,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
		}, "client-key")
		if w.Code < 400 {
			t.Fatalf("status=%d，want 4xx/5xx", w.Code)
		}
		if got := w.Header().Get("Retry-After"); got != "" {
			t.Errorf("本地错误凭空带了 Retry-After=%q", got)
		}
	})
}

type failDispatchReplay struct{}

func (failDispatchReplay) Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	return replayv1.TargetLease{}, replayv1.Error{Code: replayv1.CodeUnauthorized, Message: "no candidates"}
}
func (failDispatchReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{}, nil
}

// 超时家族必须换满整池。408/425 修复前落 ClassifyStatus 的 default 判不可重试，
// 第一个目标就放弃（dispatch=1），而同为超时的 504/524 会烧到池子见底——同一种
// 故障因状态码差一位拿到相反的调度动作。
func TestTimeoutFamilyBurnsPool(t *testing.T) {
	for _, status := range []int{408, 425, 504, 524} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var hits atomic.Int32
			up := statusUpstream(t, status, "upstream request timeout", &hits)
			defer up.Close()
			rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
			w := fwdTo(t, rp, "anthropic")
			if rp.calls != poolLimit+1 || hits.Load() != poolLimit {
				t.Fatalf("dispatch=%d upstreamHits=%d，want %d/%d（超时必须换目标重试）",
					rp.calls, hits.Load(), poolLimit+1, poolLimit)
			}
			if w.Code != status {
				t.Fatalf("客户端状态码=%d，want %d（类型反推不得改写真实码）", w.Code, status)
			}
			if !strings.Contains(w.Body.String(), ir.ErrTypeOverloaded) {
				t.Fatalf("客户端错误类型=%s，want 含 %s", w.Body.String(), ir.ErrTypeOverloaded)
			}
		})
	}
}

// 非信封错误体的端到端口径：代理插的 502、上游原生错误体、只有 message 的形状
// 都要走同一条解析（ir.ParseUpstreamError）。期望值直接取自它——客户端看到的与
// 上报给 Replay 的必须与之逐字一致，relay 若在错误路径上另造一套口径就会在这里分叉。
// 502/503 也在表里：这两档可重试，会先换目标再回落，比 4xx 多走一段调度。
// 第三条是 ModelSurge 信封形状（status 是数字）：字段类型不匹配曾让整个 error 对象
// 被丢弃，客户端拿到的 message 是一整段转义原文、code 全丢。
func TestNonEnvelopeUpstreamErrorReachesClientParsed(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"proxy gave up","type":"server_error"}}`,
		`{"error":{"code":503,"message":"upstream unavailable","status":"UNAVAILABLE"}}`,
		`{"error":{"code":"throttled","message":"slow down","retryable":true,"status":429}}`,
	} {
		for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%d/%s", status, body[:24]), func(t *testing.T) {
				want := ir.ParseUpstreamError(status, []byte(body))
				var hits atomic.Int32
				up := bodyUpstream(t, status, http.Header{"Retry-After": []string{"31"}}, body, &hits)
				defer up.Close()
				rp := &poolReplay{baseURL: up.URL, limit: 1}
				w := fwdTo(t, rp, "openai-chat")

				if len(rp.reports) == 0 {
					t.Fatalf("没有上报结果（upstreamHits=%d）", hits.Load())
				}
				if got := rp.reports[len(rp.reports)-1].Message; got != want.Message {
					t.Errorf("上报消息=%q，解析结论=%q", got, want.Message)
				}
				if w.Code != status {
					t.Errorf("客户端状态码=%d，want %d", w.Code, status)
				}
				if !strings.Contains(w.Body.String(), want.Type) {
					t.Errorf("客户端错误体缺规范类型 %q：%s", want.Type, w.Body.String())
				}
				// 转义引号是「原文整段入 Message」的特征；解析后不该再出现。
				if strings.Contains(w.Body.String(), `\"`) {
					t.Errorf("客户端错误体仍是转义原文：%s", w.Body.String())
				}
				if want.Code != "" && !strings.Contains(w.Body.String(), want.Code) {
					t.Errorf("丢了上游错误码 %q：%s", want.Code, w.Body.String())
				}
				if got := w.Header().Get("Retry-After"); got != "31" {
					t.Errorf("Retry-After=%q，want 31", got)
				}
			})
		}
	}
}
