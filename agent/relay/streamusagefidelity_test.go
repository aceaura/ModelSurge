// streamusagefidelity_test.go 流式 usage 帧的客户端 opt-in 端到端保真。
//
// 覆盖三条写出路径（普通 SSE 流、聚合转流、kiro ndjson 流）与两条不变量：
// 客户端没 opt-in 就看不到 usage-only 帧；记账上报的 usage 与客户端呈现解耦，
// 不随 opt-in 变化。
package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/config"
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// anthropicUsageUpstream 伪造一个 anthropic SSE 上游：usage 在 message_start
// （输入侧）与 message_delta（输出侧）分两处给出；withUsage=false 时两处都不给，
// 用于估算路径。正文可指定：ir.EstimateTokens 按 4 字符 1 token 粗算，
// 两三个字符会估成 0。
func anthropicUsageUpstream(t *testing.T, withUsage bool, text string) *httptest.Server {
	t.Helper()
	startUsage := `"usage":{"input_tokens":100,"cache_read_input_tokens":40}`
	deltaUsage := `"usage":{"output_tokens":5}`
	if !withUsage {
		startUsage = `"usage":{}`
		deltaUsage = ""
	}
	delta, _ := json.Marshal(text)
	frames := []string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_1","model":"claude",` + startUsage + `}}`,
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + string(delta) + `}}`,
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}`,
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"}` + commaIf(deltaUsage) + deltaUsage + `}`,
		"event: message_stop\ndata: " + `{"type":"message_stop"}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
}

func commaIf(s string) string {
	if s == "" {
		return ""
	}
	return ","
}

// chatStreamUpstream 伪造一个 openai-chat SSE 上游：内容块 + usage-only 末帧
// （choices 空数组，OpenAI 在 include_usage=true 时的形态）。同时记下收到的
// 请求体，用于验证出站是否恒注入 include_usage。
type chatStreamUpstream struct {
	gotBody []byte
	srv     *httptest.Server
}

func newChatStreamUpstream(t *testing.T, sse bool) *chatStreamUpstream {
	t.Helper()
	u := &chatStreamUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.gotBody, _ = io.ReadAll(r.Body)
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, f := range []string{
				`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
				`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
				`data: [DONE]`,
			} {
				_, _ = w.Write([]byte(f + "\n\n"))
				w.(http.Flusher).Flush()
			}
			return
		}
		// 非 SSE：上游忽略 stream=true 直接回完整 JSON（兜底路径的触发条件）。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func usageLease(protocol, baseURL string) replayv1.TargetLease {
	return replayv1.TargetLease{
		RequestID: "req", GroupID: "group", TargetID: "acct/m1", Protocol: protocol,
		NativeModel: "m1", BaseURL: baseURL, Credential: "sk-up",
	}
}

func forwardUsage(t *testing.T, rp Replay, client string, cfg *config.Config, mutate func(*ir.Request)) *httptest.ResponseRecorder {
	t.Helper()
	f := NewForwarder(cfg, rp, nil)
	w := httptest.NewRecorder()
	req := &ir.Request{
		Model: "public", MaxTokens: 64, Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}
	if mutate != nil {
		mutate(req)
	}
	f.Forward(t.Context(), w, proto.MustInbound(client), req, "client-key")
	return w
}

// Chat 客户端没 opt-in 就不得收到 usage-only 帧。该帧的 choices 是空数组，
// 按 OpenAI 契约只在 stream_options.include_usage 时出现；没要的客户端按
// choices[0] 取增量会越界。
func TestChatUsageFrameFollowsClientOptInEndToEnd(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run("optIn="+boolStr(optIn), func(t *testing.T) {
			up := anthropicUsageUpstream(t, true, "hi")
			defer up.Close()
			rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
			w := forwardUsage(t, rp, "openai-chat", &config.Config{}, func(r *ir.Request) { r.IncludeUsage = optIn })
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if got := strings.Contains(body, `"choices":[]`); got != optIn {
				t.Fatalf("usage-only 帧出现=%v，want %v\n%s", got, optIn, body)
			}
			if got := strings.Contains(body, `"prompt_tokens"`); got != optIn {
				t.Fatalf("usage 数值出现=%v，want %v\n%s", got, optIn, body)
			}
			if optIn {
				// 含缓存的总输入口径：100 + 40。
				for _, want := range []string{`"prompt_tokens":140`, `"completion_tokens":5`, `"cached_tokens":40`} {
					if !strings.Contains(body, want) {
						t.Fatalf("usage 帧缺少 %s\n%s", want, body)
					}
				}
			}
			// 正文与终止帧不受门控影响。
			for _, want := range []string{`"content":"hi"`, `"finish_reason":"stop"`, "data: [DONE]"} {
				if !strings.Contains(body, want) {
					t.Fatalf("流缺少 %s\n%s", want, body)
				}
			}
		})
	}
}

// 记账与呈现解耦：客户端没要 usage 帧，上报给 Replay 的用量仍必须完整。
// 这条是出站恒注入 include_usage 的理由——若把出站也改成跟随客户端意图，
// 没 opt-in 的请求会静默丢掉计费数据。
func TestReportedUsageIndependentOfClientOptIn(t *testing.T) {
	want := replayv1.Usage{InputTokens: 100, OutputTokens: 5, CacheRead: 40}
	for _, optIn := range []bool{false, true} {
		t.Run("optIn="+boolStr(optIn), func(t *testing.T) {
			up := anthropicUsageUpstream(t, true, "hi")
			defer up.Close()
			rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
			forwardUsage(t, rp, "openai-chat", &config.Config{}, func(r *ir.Request) { r.IncludeUsage = optIn })
			if len(rp.reports) != 1 {
				t.Fatalf("reports=%d, want 1", len(rp.reports))
			}
			if got := rp.reports[0].Usage; got != want {
				t.Fatalf("上报用量=%+v, want %+v", got, want)
			}
		})
	}
}

// 出站请求体恒带 include_usage=true，无论客户端 opt-in 与否；上游是 Chat 时，
// 它回的 usage-only 帧也不得原样漏给没 opt-in 的客户端。
func TestOutboundForcesIncludeUsageRegardlessOfClient(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		for _, sse := range []bool{true, false} {
			t.Run("optIn="+boolStr(optIn)+"/sse="+boolStr(sse), func(t *testing.T) {
				up := newChatStreamUpstream(t, sse)
				rp := &reportCaptureReplay{lease: usageLease("openai-chat", up.srv.URL)}
				w := forwardUsage(t, rp, "openai-chat", &config.Config{}, func(r *ir.Request) { r.IncludeUsage = optIn })
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				var sent struct {
					StreamOptions *struct {
						IncludeUsage bool `json:"include_usage"`
					} `json:"stream_options"`
				}
				if err := json.Unmarshal(up.gotBody, &sent); err != nil {
					t.Fatalf("出站请求体解析失败: %v\n%s", err, up.gotBody)
				}
				if sent.StreamOptions == nil || !sent.StreamOptions.IncludeUsage {
					t.Fatalf("optIn=%v: 出站没强制 include_usage：%s", optIn, up.gotBody)
				}
				// 客户端侧仍按 opt-in 门控（sse=false 走聚合转流那条写出路径）。
				body := w.Body.String()
				if got := strings.Contains(body, `"choices":[]`); got != optIn {
					t.Fatalf("usage-only 帧出现=%v，want %v\n%s", got, optIn, body)
				}
				if !strings.Contains(body, `"content":"hi"`) {
					t.Fatalf("正文丢了：%s", body)
				}
			})
		}
	}
}

// opt-in 标志只门控 Chat。其余三族的 usage 是协议内建、无条件回传，
// 标志若被误当成全局抑制开关，这三族会静默丢掉用量。
func TestUsageOptInDoesNotSuppressOtherClientProtocols(t *testing.T) {
	for _, tc := range []struct{ client, marker string }{
		// anthropic 把缓存单列，input_tokens 是不含缓存的裸输入；其余两族按
		// OpenAI/Gemini 口径给含缓存的总输入（100 + 40）。
		{"anthropic", `"input_tokens":100`},
		{"openai-responses", `"input_tokens":140`},
		{"gemini", `"promptTokenCount":140`},
	} {
		t.Run(tc.client, func(t *testing.T) {
			up := anthropicUsageUpstream(t, true, "hi")
			defer up.Close()
			rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
			// IncludeUsage 保持 false：这三族不该受它影响。
			w := forwardUsage(t, rp, tc.client, &config.Config{}, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.marker) {
				t.Fatalf("%s 客户端丢了 usage（marker=%s）：\n%s", tc.client, tc.marker, w.Body.String())
			}
		})
	}
}

// kiro ndjson 写出路径同样受门控：三个出口任一漏传客户端意图都会在这里暴露。
func TestKiroStreamPathRespectsUsageOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run("optIn="+boolStr(optIn), func(t *testing.T) {
			rp := &kiroExecuteReplay{
				lease: replayv1.TargetLease{RequestID: "req", GroupID: "group", TargetID: "kiro/public", Protocol: "kiro"},
				body:  kiroUsageNDJSON(t),
			}
			w := forwardUsage(t, rp, "openai-chat", &config.Config{}, func(r *ir.Request) {
				r.Model = "public"
				r.IncludeUsage = optIn
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if got := strings.Contains(body, `"choices":[]`); got != optIn {
				t.Fatalf("kiro 路径 usage-only 帧出现=%v，want %v\n%s", got, optIn, body)
			}
			if !strings.Contains(body, `"content":"hi"`) {
				t.Fatalf("kiro 路径正文丢了：%s", body)
			}
		})
	}
}

func kiroUsageNDJSON(t *testing.T) io.ReadCloser {
	t.Helper()
	var buf bytes.Buffer
	start := ir.Usage{InputTokens: 100, CacheReadTokens: 40}
	delta := ir.Usage{OutputTokens: 5}
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "public", Usage: &start},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &delta},
		{Type: ir.EvMessageStop},
	} {
		if err := json.NewEncoder(&buf).Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes()))
}

// 估算用量（estimate_usage 开启且上游没报）不得推给没 opt-in 的客户端：那是
// 本地按字符数粗估的值，客户端当成上游实测数拿去计费会算错。
func TestEstimatedUsageNotShownToClientWithoutOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run("optIn="+boolStr(optIn), func(t *testing.T) {
			up := anthropicUsageUpstream(t, false, strings.Repeat("estimated output ", 20))
			defer up.Close()
			rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
			w := forwardUsage(t, rp, "openai-chat", &config.Config{EstimateUsage: true},
				func(r *ir.Request) { r.IncludeUsage = optIn })
			body := w.Body.String()
			if got := strings.Contains(body, `"prompt_tokens"`); got != optIn {
				t.Fatalf("估算 usage 出现=%v，want %v\n%s", got, optIn, body)
			}
			// 估算值按设计不进记账（attempt 里的 onUsage 回调见到 Estimated 直接
			// 返回）：Replay 拿上报用量做额度与调度，粗估数字进去会污染账。所以
			// 估算只影响日志摘要与（opt-in 时的）客户端呈现，这半边也锁住。
			if len(rp.reports) != 1 {
				t.Fatalf("reports=%d, want 1", len(rp.reports))
			}
			if got := rp.reports[0].Usage; got != (replayv1.Usage{}) {
				t.Fatalf("optIn=%v: 估算用量被上报了：%+v", optIn, got)
			}
		})
	}
}

// 思考抑制装饰器包在最外层，proto.NewClientStreamEncoder 只对最外层做类型
// 断言：装饰器不转发 opt-in 的话，隐藏思考 + Chat opt-in 同时出现时 usage 帧
// 会静默消失。
// 两态都要跑：只测 opt-in=true 的话，装饰器把意图硬编码成 true 也看不出来。
func TestHiddenThoughtsDecoratorForwardsUsageOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run("optIn="+boolStr(optIn), func(t *testing.T) {
			up := anthropicUsageUpstream(t, true, "hi")
			defer up.Close()
			rp := &reportCaptureReplay{lease: usageLease("anthropic", up.URL)}
			w := forwardUsage(t, rp, "openai-chat", &config.Config{}, func(r *ir.Request) {
				r.IncludeUsage = optIn
				r.Thinking = &ir.ThinkingConfig{HideThoughts: true}
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if got := strings.Contains(body, `"choices":[]`); got != optIn {
				t.Fatalf("装饰器没有透传 opt-in，usage 帧出现=%v，want %v\n%s", got, optIn, body)
			}
			if optIn && !strings.Contains(body, `"prompt_tokens":140`) {
				t.Fatalf("usage 数值不对：\n%s", body)
			}
		})
	}
}

// 严格 tool_choice 分支走 attemptKiroStrict：与 streamKiroToClient 完全独立的第二个
// kiro 出口（恒聚合后由 writeResponse 合成事件流），opt-in 得在这条路上同样生效。
// 夹具须返回一次命名的 tool_use，否则策略校验不过，压根走不到写出那一步。
func TestKiroStrictPathRespectsUsageOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run("optIn="+boolStr(optIn), func(t *testing.T) {
			rp := &kiroExecuteReplay{
				lease: replayv1.TargetLease{RequestID: "req", GroupID: "group", TargetID: "kiro/public", Protocol: "kiro"},
				body:  kiroStrictUsageNDJSON(t),
			}
			w := forwardUsage(t, rp, "openai-chat", &config.Config{}, func(r *ir.Request) {
				r.Model = "public"
				r.IncludeUsage = optIn
				r.Tools = []ir.Tool{{Name: "t", Description: "d"}}
				r.ToolChoice = &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "t"}
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if got := strings.Contains(body, `"choices":[]`); got != optIn {
				t.Fatalf("kiro 严格分支 usage-only 帧出现=%v，want %v\n%s", got, optIn, body)
			}
			if !strings.Contains(body, `"name":"t"`) {
				t.Fatalf("kiro 严格分支工具调用丢了：%s", body)
			}
		})
	}
}

func kiroStrictUsageNDJSON(t *testing.T) io.ReadCloser {
	t.Helper()
	var buf bytes.Buffer
	start := ir.Usage{InputTokens: 100, CacheReadTokens: 40}
	delta := ir.Usage{OutputTokens: 5}
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "public", Usage: &start},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "tu_1", Name: "t", Input: json.RawMessage(`{}`)}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse, Usage: &delta},
		{Type: ir.EvMessageStop},
	} {
		if err := json.NewEncoder(&buf).Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes()))
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
