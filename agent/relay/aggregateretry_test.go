package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// 非流式（聚合）路径上「流内错误该不该换账号重试」的判定。
//
// 未写任何字节 → 换上游重发是**安全**的；但安全不等于有用。非法请求 / 内容过滤
// 换谁都会被同样拒绝，一律置可重试会让调度器把整个账号池烧一遍才停下（实测一个
// invalid_request 打满全部目标）。可重试性因此按规范类型判（ir.StreamRetryable，
// 与解码器共用同一张表），传输与解码失败仍是 upstream_error → 可重试。

// poolReplay 每次 Dispatch 发一个新目标，超过 limit 就报池子空了：这样「烧了几个
// 账号」变成可数的，也不会像忽略 TriedIDs 的夹具那样让重试循环挂死。
type poolReplay struct {
	protocol string
	baseURL  string
	ndjson   func(call int) io.ReadCloser
	limit    int
	calls    int
}

func (r *poolReplay) Dispatch(context.Context, replayv1.DispatchRequest) (replayv1.TargetLease, error) {
	r.calls++
	if r.calls > r.limit {
		return replayv1.TargetLease{}, fmt.Errorf("pool exhausted after %d targets", r.limit)
	}
	lease := replayv1.TargetLease{RequestID: "req", GroupID: "g", TargetID: fmt.Sprintf("t%d", r.calls)}
	if r.protocol == "kiro" {
		lease.Protocol = "kiro"
		lease.NativeModel = "public"
		return lease, nil
	}
	lease.Protocol = "anthropic"
	lease.NativeModel = "native"
	lease.BaseURL = r.baseURL
	lease.Credential = "sk-up"
	return lease, nil
}

func (*poolReplay) Report(context.Context, replayv1.ResultReport) (replayv1.ResultResponse, error) {
	return replayv1.ResultResponse{Applied: true}, nil
}

func (*poolReplay) WebSearch(context.Context, replayv1.WebSearchRequest) (replayv1.WebSearchResponse, error) {
	return replayv1.WebSearchResponse{}, nil
}

func (r *poolReplay) ExecuteKiro(context.Context, replayv1.KiroExecuteRequest) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
		Body:       r.ndjson(r.calls),
	}, nil
}

func poolForward(t *testing.T, rp *poolReplay, model string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	f := NewForwarder(&config.Config{}, rp, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: model, MaxTokens: 64, Stream: stream,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	return w
}

func sseUpstream(t *testing.T, frames []string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, fr := range frames {
			_, _ = w.Write([]byte(fr + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(up.Close)
	return up
}

const (
	poolLimit = 5

	poolStart = `event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"native","content":[],"usage":{"input_tokens":11}}}`
	poolText = `event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
)

func poolErr(typ, msg string) string {
	return `event: error` + "\n" +
		fmt.Sprintf(`data: {"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
}

// 换谁都会失败的错误只许打一个目标；换一个账号可能成功的错误必须烧到池子见底。
func TestAggregateStreamErrorRetryFollowsCanonicalType(t *testing.T) {
	for _, tc := range []struct {
		errType      string
		wantDispatch int
		wantUpstream int
	}{
		{ir.ErrTypeInvalidReq, 1, 1},
		{ir.ErrTypeContentFilter, 1, 1},
		{ir.ErrTypeNotFound, 1, 1},
		{ir.ErrTypeRateLimit, poolLimit + 1, poolLimit},
		{ir.ErrTypeOverloaded, poolLimit + 1, poolLimit},
		{ir.ErrTypeUpstream, poolLimit + 1, poolLimit},
	} {
		t.Run(tc.errType, func(t *testing.T) {
			var hits atomic.Int32
			up := sseUpstream(t, []string{poolStart, poolText, poolErr(tc.errType, "boom")}, &hits)
			rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
			w := poolForward(t, rp, "m", false)

			if rp.calls != tc.wantDispatch {
				t.Errorf("Dispatch 调用数 = %d，应为 %d（池上限 %d）", rp.calls, tc.wantDispatch, poolLimit)
			}
			if int(hits.Load()) != tc.wantUpstream {
				t.Errorf("上游被打 %d 次，应为 %d", hits.Load(), tc.wantUpstream)
			}
			if !strings.Contains(w.Body.String(), tc.errType) {
				t.Errorf("客户端没看到规范类型 %q：body=%.200s", tc.errType, w.Body.String())
			}
		})
	}
}

// 传输与解码失败必须保持可重试：这是「强制可重试」原本要保的那一半，改掉写死的
// true 之后由 upstream_error 类型继续保住。
func TestAggregateDecodeFailureStillSwitchesTargets(t *testing.T) {
	var hits atomic.Int32
	up := sseUpstream(t, []string{poolStart, `event: content_block_delta` + "\n" + `data: {截断的畸形 JSON`}, &hits)
	rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
	poolForward(t, rp, "m", false)

	if rp.calls != poolLimit+1 {
		t.Errorf("解码失败应换目标重发到池子见底：Dispatch=%d hits=%d", rp.calls, hits.Load())
	}
}

// 对照：真断流（没有任何错误事件）不是错误，照常聚合出响应，一个目标就够。
// 这条守住 R48 的中断档语义没有被本轮改动带偏。
func TestAggregateAbortedStreamStillCompletes(t *testing.T) {
	var hits atomic.Int32
	up := sseUpstream(t, []string{poolStart, poolText}, &hits)
	rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
	w := poolForward(t, rp, "m", false)

	if w.Code != http.StatusOK {
		t.Fatalf("真断流不该报错：status=%d body=%s", w.Code, w.Body.String())
	}
	if rp.calls != 1 || hits.Load() != 1 {
		t.Errorf("正常聚合不该重发：Dispatch=%d hits=%d", rp.calls, hits.Load())
	}
	if !strings.Contains(w.Body.String(), `"stop_reason":"max_tokens"`) {
		t.Errorf("中断档丢了：body=%s", w.Body.String())
	}
}

// 已写字节就不许再换目标：流式路径的重试窗口在首字节之前，之后只能把错误如实下发。
func TestStreamedErrorNeverRetriesAfterBytesWritten(t *testing.T) {
	var hits atomic.Int32
	up := sseUpstream(t, []string{poolStart, poolText, poolErr(ir.ErrTypeInvalidReq, "boom")}, &hits)
	rp := &poolReplay{baseURL: up.URL, limit: poolLimit}
	w := poolForward(t, rp, "m", true)

	if rp.calls != 1 || hits.Load() != 1 {
		t.Errorf("流式路径已写字节后仍在重发：Dispatch=%d hits=%d", rp.calls, hits.Load())
	}
	if !strings.Contains(w.Body.String(), ir.ErrTypeInvalidReq) {
		t.Errorf("错误没下发给客户端：body=%.300s", w.Body.String())
	}
}

func kiroNDJSON(events ...ir.Event) io.ReadCloser {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			panic(err)
		}
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes()))
}

// kiro 数据面的可重试性归 Upstream 判：它手上有 ClassifyKiroError，认得 kiro 的
// 原因码，agent 只看得到规范类型。今天两者等价（Upstream 的流内 EvError 恒为
// upstream_error + 可重试），这条锁的是「agent 不再按类型覆盖 Upstream 的结论」，
// 两个方向都测，防将来被"顺手统一"。
//
// 注意 fixture 不带 Reason：kiro 的 401/403/402/INVALID_MODEL_ID 由
// localResultAction 的专属分支直接决定动作，根本不查 Retryable，带上就测不到这一轴。
func TestKiroAggregateErrorHonorsUpstreamRetryable(t *testing.T) {
	// 两种到达形态走的是 aggregateKiro 里两个不同的出口：首事件即错误 → 错误经
	// aggregator.Finish() 出来；错误在流中途 → 循环里直接 return。两处都不许覆盖。
	for _, shape := range []struct {
		label  string
		events func(err ir.Event) []ir.Event
	}{
		{"首事件即错误", func(err ir.Event) []ir.Event { return []ir.Event{err} }},
		{"错误在流中途", func(err ir.Event) []ir.Event {
			return []ir.Event{
				{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
				{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
				err,
			}
		}},
	} {
		for _, tc := range []struct {
			label        string
			retryable    bool
			wantDispatch int
		}{
			{"upstream 判不可重试", false, 1},
			{"upstream 判可重试", true, poolLimit + 1},
		} {
			t.Run(shape.label+"/"+tc.label, func(t *testing.T) {
				rp := &poolReplay{
					protocol: "kiro",
					limit:    poolLimit,
					ndjson: func(int) io.ReadCloser {
						return kiroNDJSON(shape.events(ir.Event{Type: ir.EvError, Err: &ir.Error{
							StatusCode: http.StatusBadRequest,
							Type:       ir.ErrTypeUpstream,
							Message:    "boom",
							Retryable:  tc.retryable,
						}})...)
					},
				}
				w := poolForward(t, rp, "public", false)
				if rp.calls != tc.wantDispatch {
					t.Errorf("Dispatch 调用数 = %d，应为 %d", rp.calls, tc.wantDispatch)
				}
				if !strings.Contains(w.Body.String(), "boom") {
					t.Errorf("错误没下发给客户端：body=%.300s", w.Body.String())
				}
			})
		}
	}
}
