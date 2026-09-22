package openairesponses

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// 真实 wire 样本取自 sub2api backend/internal/service/openai_capacity_shed_test.go:228
// 与 openai_gateway_response_failed_passthrough_test.go:141（模拟 OpenAI 真实上游：
// 过载时先发裸 error，再跟 response.failed）。
const (
	errCreated   = `{"type":"response.created","response":{"id":"r1","object":"response","created_at":1,"model":"m","status":"in_progress"}}`
	errOverload  = `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded."},"sequence_number":2}`
	errCyber     = `{"type":"error","error":{"code":"cyber_policy","message":"blocked by cyber policy"}}`
	errFilterTyp = `{"type":"error","error":{"type":"content_filter","message":"filtered"}}`
	errNested    = `{"type":"error","response":{"error":{"code":"nested_code","message":"nested msg"}}}`
	errBare      = `{"type":"error"}`
	errDelta     = `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"前半"}`
	errCompleted = `{"type":"response.completed","response":{"id":"r1","object":"response","created_at":1,"model":"m","status":"completed","output":[]}}`
	errIncompl   = `{"type":"response.incomplete","response":{"id":"r1","object":"response","created_at":1,"model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`
	errFailed    = `{"type":"response.failed","response":{"id":"r1","object":"response","created_at":1,"model":"m","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}}`
	errFailedNoT = `{"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"c1","message":"m1"}}}`
)

// errsOf 收齐流里的 EvError。
func errsOf(evs []ir.Event) []*ir.Error {
	var out []*ir.Error
	for _, ev := range evs {
		if ev.Type == ir.EvError {
			out = append(out, ev.Err)
		}
	}
	return out
}

func countType(evs []ir.Event, t ir.EventType) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == t {
			n++
		}
	}
	return n
}

// 裸 error 事件的错误体在顶层，不在 response.error 下。此前只读后者，上游给的
// type/code/message 三个字段全部静默丢失，客户端只看到一个空错误。
func TestStreamDecodeBareErrorCarriesTopLevelDetails(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated, errOverload))
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", len(errs))
	}
	e := errs[0]
	if e.Type != "service_unavailable_error" || e.Code != "server_is_overloaded" ||
		!strings.Contains(e.Message, "overloaded") {
		t.Fatalf("错误详情丢失：type=%q code=%q msg=%q", e.Type, e.Code, e.Message)
	}
	if !e.Retryable {
		t.Errorf("上游过载应可重试，实得 retryable=false")
	}
}

// 风控拦截判成可重试，调度器会换目标重发一个永远不可能成功的请求，把账号池白烧一遍。
func TestStreamDecodeBareErrorCyberPolicyNotRetryable(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated, errCyber))
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", len(errs))
	}
	if errs[0].Retryable {
		t.Errorf("cyber_policy 必须不可重试")
	}
	if errs[0].Type != ir.ErrTypeContentFilter {
		t.Errorf("cyber_policy 应归为 %q，实得 %q", ir.ErrTypeContentFilter, errs[0].Type)
	}
}

// content_filter 类型走同一套特判，不只认 cyber_policy 这一个码。
func TestStreamDecodeBareErrorContentFilterTypeNotRetryable(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated, errFilterTyp))
	if len(errs) != 1 || errs[0].Retryable {
		t.Fatalf("content_filter 应不可重试：%+v", errs)
	}
	if errs[0].Type != ir.ErrTypeContentFilter {
		t.Errorf("应归为 %q，实得 %q", ir.ErrTypeContentFilter, errs[0].Type)
	}
}

// 非法请求换谁都会被同样拒绝；可重试性按类型判，与 anthropic / chat 共用同一张表。
func TestStreamDecodeInvalidRequestNotRetryable(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated,
		`{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`))
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", len(errs))
	}
	if errs[0].Retryable {
		t.Errorf("invalid_request_error 必须不可重试")
	}
}

// 少数上游把错误挂在 response.error 下（本仓此前的生成形态），回落必须还在。
func TestStreamDecodeErrorFallsBackToResponseError(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated, errNested))
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", len(errs))
	}
	if errs[0].Code != "nested_code" || errs[0].Message != "nested msg" {
		t.Fatalf("response.error 回落失效：code=%q msg=%q", errs[0].Code, errs[0].Message)
	}
}

// 错误体缺席时不得把规范类型覆盖成空串：type 为空，下游 RenderStreamError 产出
// 的错误帧就没有规范类型，客户端无从判断该不该重试。
func TestStreamDecodeErrorWithoutDetailsKeepsCanonicalType(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated, errBare))
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", len(errs))
	}
	if errs[0].Type != ir.ErrTypeUpstream {
		t.Errorf("应回落 %q，实得 %q", ir.ErrTypeUpstream, errs[0].Type)
	}
	if errs[0].Message == "" {
		t.Errorf("错误消息不得为空")
	}
}

func TestStreamDecodeFailedWithoutErrorTypeKeepsCanonicalType(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated, errFailedNoT))
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", len(errs))
	}
	if errs[0].Type != ir.ErrTypeUpstream {
		t.Errorf("应回落 %q，实得 %q", ir.ErrTypeUpstream, errs[0].Type)
	}
	if errs[0].Code != "c1" || errs[0].Message != "m1" {
		t.Errorf("已有的 code/message 不得丢：code=%q msg=%q", errs[0].Code, errs[0].Message)
	}
}

// 裸 error 的同一件事：错误体在但 type 缺席。
func TestStreamDecodeBareErrorWithoutTypeKeepsCanonicalType(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated, `{"type":"error","error":{"code":"c2","message":"m2"}}`))
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", len(errs))
	}
	if errs[0].Type != ir.ErrTypeUpstream {
		t.Errorf("应回落 %q，实得 %q", ir.ErrTypeUpstream, errs[0].Type)
	}
	if errs[0].Code != "c2" || errs[0].Message != "m2" {
		t.Errorf("已有的 code/message 不得丢：code=%q msg=%q", errs[0].Code, errs[0].Message)
	}
}

// 错误体在但 message 缺席时保留兜底文案：errorBody.Message 没有 omitempty，
// 覆盖成空会让客户端拿到一个 message 为空串的错误帧，无从判断发生了什么。
func TestStreamDecodeErrorWithEmptyMessageKeepsFallback(t *testing.T) {
	errs := errsOf(feedFrames(t, errCreated, `{"type":"error","error":{"code":"c3"}}`))
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", len(errs))
	}
	if errs[0].Message == "" {
		t.Errorf("消息缺席时不得覆盖成空串")
	}
	if errs[0].Code != "c3" {
		t.Errorf("已有的 code 不得丢：code=%q", errs[0].Code)
	}
}

// 错误之后照常产出 StopEndTurn 的终止事件，下游编码器会补一个 response.completed，
// 客户端把失败读成正常结束。
func TestStreamDecodeErrorThenCompletedDoesNotFakeNormalEnd(t *testing.T) {
	evs := feedFrames(t, errCreated, errDelta, errOverload, errCompleted)
	if n := countType(evs, ir.EvError); n != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d", n)
	}
	if n := countType(evs, ir.EvMessageDelta); n != 0 {
		t.Errorf("错误之后不得再有 message_delta（会把错误伪装成正常结束），实得 %d 个：%v", n, types(evs))
	}
	if n := countType(evs, ir.EvMessageStop); n != 0 {
		t.Errorf("错误之后不得再有 message_stop，实得 %d 个", n)
	}
}

// incomplete 变体：把上游过载读成「输出超长」，客户端会去加大 max_output_tokens。
func TestStreamDecodeErrorThenIncompleteDoesNotFakeTruncation(t *testing.T) {
	evs := feedFrames(t, errCreated, errDelta, errOverload, errIncompl)
	if n := countType(evs, ir.EvMessageDelta); n != 0 {
		t.Errorf("错误之后不得再有 message_delta：%v", types(evs))
	}
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta && ev.StopReason == ir.StopMaxTokens {
			t.Fatalf("上游过载被读成输出超长：%v", types(evs))
		}
	}
}

// 真实上游的序列就是 error 之后再跟 response.failed，同一个错误不得下发两遍。
func TestStreamDecodeErrorThenFailedEmitsErrorOnce(t *testing.T) {
	evs := feedFrames(t, errCreated, errOverload, errFailed)
	errs := errsOf(evs)
	if len(errs) != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d：%v", len(errs), types(evs))
	}
	if errs[0].Code != "server_is_overloaded" {
		t.Errorf("保留的应是首个错误的详情，实得 code=%q", errs[0].Code)
	}
}

func TestStreamDecodeFailedThenErrorEmitsErrorOnce(t *testing.T) {
	evs := feedFrames(t, errCreated, errFailed, errOverload)
	if n := countType(evs, ir.EvError); n != 1 {
		t.Fatalf("期望 1 个 EvError，实得 %d：%v", n, types(evs))
	}
}

// error 已是终止事件，Finish() 不得再补 StopAborted 的终止帧。
func TestStreamDecodeErrorIsTerminalForFinish(t *testing.T) {
	evs := feedFrames(t, errCreated, errDelta, errOverload)
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			t.Fatalf("断流兜底不得在错误之后再补终止事件：%v", types(evs))
		}
	}
}

// 官方 SDK 的 ErrorEvent 只读顶层 error；挂在 response 下等于错误对客户端不可见。
func TestStreamEncodeErrorFrameUsesTopLevelError(t *testing.T) {
	frame := string(codec{}.RenderStreamError(&ir.Error{
		Type: ir.ErrTypeContentFilter, Code: "cyber_policy", Message: "blocked by cyber policy",
	}))
	if !strings.Contains(frame, `"error":{"code":"cyber_policy"`) {
		t.Fatalf("错误体必须在顶层：%s", frame)
	}
	if strings.Contains(frame, `"response"`) {
		t.Errorf("不得带 response 壳（会多出 id/model 全空的畸形对象）：%s", frame)
	}
	if !strings.Contains(frame, ir.ErrTypeContentFilter) {
		t.Errorf("缺规范类型：%s", frame)
	}
}

// 编码器收到 EvError 后不得再凭空补终止帧：那时 stopReason 还是零值，补出来的是
// response.incomplete 带 reason=max_output_tokens——上游过载被翻译成「你输出超长了」。
func TestStreamEncodeErrorNotFollowedByFakeTerminal(t *testing.T) {
	out := encodeStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
		ir.Event{Type: ir.EvError, Err: &ir.Error{Type: "service_unavailable_error",
			Code: "server_is_overloaded", Message: "overloaded", Retryable: true}},
	)
	if strings.Contains(out, "max_output_tokens") {
		t.Fatalf("错误被翻译成输出超长：%s", out)
	}
	if strings.Contains(out, "response.completed") || strings.Contains(out, "response.incomplete") {
		t.Fatalf("错误之后不得有正常终止帧：%s", out)
	}
	if strings.Count(out, `"type":"error"`) != 1 {
		t.Fatalf("期望恰好 1 个 error 帧：%s", out)
	}
}

// 但开着的块仍要关掉，否则客户端留下一个永不结束的 item。
func TestStreamEncodeErrorStillClosesOpenBlocks(t *testing.T) {
	out := encodeStream(t,
		ir.Event{Type: ir.EvMessageStart, MessageID: "r1", Model: "m"},
		ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		ir.Event{Type: ir.EvTextDelta, Index: 0, Text: "前半"},
		ir.Event{Type: ir.EvError, Err: &ir.Error{Type: ir.ErrTypeUpstream, Message: "boom", Retryable: true}},
	)
	if !strings.Contains(out, "response.output_item.done") {
		t.Fatalf("错误后未关闭开着的块：%s", out)
	}
	if strings.Contains(out, "response.completed") {
		t.Fatalf("不得补正常终止帧：%s", out)
	}
}

// 端到端：真实 wire 的过载错误 -> IR -> Responses SSE，详情保真且不伪装成截断。
func TestResponsesRoundTripOverloadStaysError(t *testing.T) {
	evs := feedFrames(t, errCreated, errDelta, errOverload)
	out := encodeStream(t, evs...)
	if !strings.Contains(out, `"code":"server_is_overloaded"`) {
		t.Fatalf("错误码在往返中丢失：%s", out)
	}
	if !strings.Contains(out, "service_unavailable_error") {
		t.Fatalf("错误类型在往返中丢失：%s", out)
	}
	if strings.Contains(out, "max_output_tokens") || strings.Contains(out, "response.completed") {
		t.Fatalf("上游过载被伪装成正常结束/输出超长：%s", out)
	}
}

// 端到端：风控拦截必须保持不可重试的语义，且错误对官方 SDK 可见。
func TestResponsesRoundTripCyberPolicyKeepsFilterSemantics(t *testing.T) {
	evs := feedFrames(t, errCreated, errCyber)
	errs := errsOf(evs)
	if len(errs) != 1 || errs[0].Retryable {
		t.Fatalf("风控拦截应不可重试：%+v", errs)
	}
	out := encodeStream(t, evs...)
	if !strings.Contains(out, ir.ErrTypeContentFilter) {
		t.Fatalf("往返后丢了规范类型：%s", out)
	}
	if !strings.Contains(out, `"error":{"code":"cyber_policy"`) {
		t.Fatalf("错误体不在顶层，官方 SDK 读不到：%s", out)
	}
}
