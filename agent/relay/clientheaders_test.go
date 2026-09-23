// clientheaders_test.go anthropic-beta 从客户端到上游的转交保真。
package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

func TestParseAnthropicBetas(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"没给", nil, ""},
		{"空串", []string{""}, ""},
		{"单个", []string{"context-1m-2025-08-07"}, "context-1m-2025-08-07"},
		{"逗号列表带空白", []string{" interleaved-thinking-2025-05-14 , context-1m-2025-08-07 "},
			"interleaved-thinking-2025-05-14,context-1m-2025-08-07"},
		{"多行合并", []string{"claude-code-20250219", "context-1m-2025-08-07"},
			"claude-code-20250219,context-1m-2025-08-07"},
		{"去重保序", []string{"b-1,a-2", "a-2,b-1"}, "b-1,a-2"},
		// oauth beta 只对 Claude 订阅 OAuth 上游有意义；本仓 anthropic 账号一律
		// api-key，转交过去只会招 4xx。
		{"剔除 oauth", []string{"oauth-2025-04-20,context-1m-2025-08-07"}, "context-1m-2025-08-07"},
		{"只有 oauth", []string{"oauth-2025-04-20"}, ""},
		// 头值最终要拼回出站头，空白与控制字符是注入面。
		{"剔除含空白", []string{"a-1 b-2,ok-3"}, "ok-3"},
		{"剔除控制字符", []string{"evil\r\nX-Injected: 1,ok-3"}, "ok-3"},
		{"剔除空段", []string{",,ok-3,,"}, "ok-3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(parseAnthropicBetas(c.lines), ",")
			if got != c.want {
				t.Fatalf("parseAnthropicBetas(%q) = %q, want %q", c.lines, got, c.want)
			}
		})
	}
}

// beta 是 Anthropic 私有词汇：转交给非 anthropic wire 的上游轻则被忽略、重则 400。
func TestApplyClientWireHeadersOnlyOnAnthropicUpstream(t *testing.T) {
	for _, protoName := range []string{"anthropic", "openai-chat", "openai-responses", "codex", "gemini", "kiro"} {
		t.Run(protoName, func(t *testing.T) {
			ctx := WithClientHeaders(context.Background(), http.Header{
				"Anthropic-Beta": []string{"context-1m-2025-08-07"},
			})
			dst := http.Header{}
			applyClientWireHeaders(ctx, dst, protoName)
			got := dst.Get("anthropic-beta")
			want := ""
			if protoName == "anthropic" {
				want = "context-1m-2025-08-07"
			}
			if got != want {
				t.Fatalf("anthropic-beta=%q, want %q", got, want)
			}
		})
	}
}

// 账号级自定义头里钉死的 beta 不能被客户端顶掉，客户端要的也不能因为账号钉了
// 一个就整条丢掉——合并去重。
func TestClientBetasMergeWithAccountHeaders(t *testing.T) {
	ctx := WithClientHeaders(context.Background(), http.Header{
		"Anthropic-Beta": []string{"context-1m-2025-08-07,claude-code-20250219"},
	})
	dst := http.Header{}
	dst.Set("anthropic-beta", "pinned-by-operator-2026-01-01,context-1m-2025-08-07")
	applyClientWireHeaders(ctx, dst, "anthropic")
	got := dst.Values("anthropic-beta")
	if len(got) != 1 {
		t.Fatalf("应合并成一行，实际 %v", got)
	}
	for _, want := range []string{"pinned-by-operator-2026-01-01", "context-1m-2025-08-07", "claude-code-20250219"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("合并结果缺 %q：%q", want, got[0])
		}
	}
	if n := strings.Count(got[0], "context-1m-2025-08-07"); n != 1 {
		t.Errorf("重复 token 没去重（出现 %d 次）：%q", n, got[0])
	}
}

// 客户端没要任何 beta 时不得在出站请求上留下 anthropic-beta 这个键：Set(k, "") 会
// 产生一个空值头，上游可能读成「显式关闭全部 beta」，与「没提」不是一回事。
func TestApplyClientWireHeadersNoKeyWithoutBetas(t *testing.T) {
	dst := http.Header{}
	applyClientWireHeaders(context.Background(), dst, "anthropic")
	applyClientWireHeaders(WithClientHeaders(context.Background(), http.Header{
		"Anthropic-Beta": []string{"oauth-2025-04-20"},
	}), dst, "anthropic")
	if vals, ok := dst["Anthropic-Beta"]; ok {
		t.Fatalf("不该出现 anthropic-beta 键，实际 %v", vals)
	}
}

// betaUpstream 记录收到的 anthropic-beta，按入站路径回一个对应协议的最小非流式响应
// （非 anthropic 的对照组必须能正常走完解码，否则失败原因就分不清是「头被转交了」
// 还是「响应压根没解出来」）。
//
// 记的是 Header.Values 的原始切片而不是 Get 的结果：「没有这个头」与「有一个空值的
// 头」在 Get 下都是 ""，但后者是真实的 wire 差异——上游可能把空的 anthropic-beta
// 读成「显式关闭全部 beta」。
type betaUpstream struct {
	mu  sync.Mutex
	got [][]string
	srv *httptest.Server
}

func newBetaUpstream(t *testing.T) *betaUpstream {
	t.Helper()
	u := &betaUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.got = append(u.got, r.Header.Values("anthropic-beta"))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// last 返回上游最后一次收到的 anthropic-beta 全部取值；键不存在时是空切片。
func (u *betaUpstream) last(t *testing.T) []string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.got) == 0 {
		t.Fatal("上游没被调用")
	}
	return u.got[len(u.got)-1]
}

func forwardWithBeta(t *testing.T, protocol, baseURL, betaHeader string) *httptest.ResponseRecorder {
	t.Helper()
	return forwardWithLease(t, usageLease(protocol, baseURL), betaHeader)
}

func forwardWithLease(t *testing.T, lease replayv1.TargetLease, betaHeader string) *httptest.ResponseRecorder {
	t.Helper()
	rp := &reportCaptureReplay{lease: lease}
	f := NewForwarder(&config.Config{}, rp, nil)
	w := httptest.NewRecorder()
	ctx := context.Background()
	if betaHeader != "" {
		ctx = WithClientHeaders(ctx, http.Header{"Anthropic-Beta": []string{betaHeader}})
	}
	f.Forward(ctx, w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "public", MaxTokens: 32,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	return w
}

// 端到端：客户端要的 1M 上下文 beta 必须真的落到上游请求头上。丢了的话上游按
// 默认 200k 窗口处理，长上下文请求被拒——客户端看不到任何说明。
func TestClientBetaReachesAnthropicUpstream(t *testing.T) {
	up := newBetaUpstream(t)
	w := forwardWithBeta(t, "anthropic", up.srv.URL, "context-1m-2025-08-07,oauth-2025-04-20")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := strings.Join(up.last(t), ","); got != "context-1m-2025-08-07" {
		t.Fatalf("上游收到的 anthropic-beta=%q, want %q", got, "context-1m-2025-08-07")
	}
}

// 没给 beta 时不得凭空造一个空头（Set(k, "") 会留下一个空值头，上游可能当成
// 「显式关闭全部 beta」）。
func TestNoBetaHeaderWhenClientSentNone(t *testing.T) {
	up := newBetaUpstream(t)
	w := forwardWithBeta(t, "anthropic", up.srv.URL, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := up.last(t); len(got) != 0 {
		t.Fatalf("客户端没给 beta，上游却收到 anthropic-beta=%q", got)
	}
}

// 非 anthropic wire 的上游不得收到这个头。
func TestBetaNotForwardedToNonAnthropicUpstream(t *testing.T) {
	up := newBetaUpstream(t)
	w := forwardWithBeta(t, "openai-chat", up.srv.URL, "context-1m-2025-08-07")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := up.last(t); len(got) != 0 {
		t.Fatalf("openai-chat 上游收到了 anthropic-beta=%q", got)
	}
}

// 账号级自定义头是 openUpstream 先 Set 到出站请求上的，转交逻辑必须排在其后——
// 顺序反了运维钉死的 beta 会把客户端要的整条顶掉。单元测试直接调
// applyClientWireHeaders 看不出这个次序，得走完整 Forward。
func TestClientBetasSurviveAccountPinnedHeaderEndToEnd(t *testing.T) {
	up := newBetaUpstream(t)
	lease := usageLease("anthropic", up.srv.URL)
	lease.Headers = map[string]string{"anthropic-beta": "pinned-by-operator-2026-01-01"}
	w := forwardWithLease(t, lease, "context-1m-2025-08-07")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := strings.Join(up.last(t), ",")
	for _, want := range []string{"pinned-by-operator-2026-01-01", "context-1m-2025-08-07"} {
		if !strings.Contains(got, want) {
			t.Errorf("上游收到的 anthropic-beta=%q，缺 %q", got, want)
		}
	}
}
