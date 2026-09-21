package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
	"github.com/aceaura/ModelSurge/agent/proto/anthropic"
	"github.com/aceaura/ModelSurge/agent/proto/gemini"
	"github.com/aceaura/ModelSurge/agent/proto/openaichat"
)

// stopreason_test.go 停止原因保真。
//
// 此前 IR 只有四档，Anthropic 的 stop_sequence 与 pause_turn 都塌成 end_turn，
// 命中的停止串整个丢失；Responses 的 incomplete_details.reason 从不读取，
// 风控拦截被当成输出超长。这些都是「内容对了但收尾语义错了」的一类。

// ---- Anthropic 映射表 ----

func TestAnthropicStopReasonRoundTrip(t *testing.T) {
	for wire, want := range map[string]ir.StopReason{
		"end_turn":      ir.StopEndTurn,
		"max_tokens":    ir.StopMaxTokens,
		"tool_use":      ir.StopToolUse,
		"refusal":       ir.StopRefusal,
		"stop_sequence": ir.StopStopSequence,
		"pause_turn":    ir.StopPauseTurn,
	} {
		t.Run(wire, func(t *testing.T) {
			if got := anthropic.MapStopReason(wire); got != want {
				t.Errorf("MapStopReason(%q) = %q, want %q", wire, got, want)
			}
			if back := anthropic.UnmapStopReason(want); back != wire {
				t.Errorf("往返丢档：%q -> %q -> %q", wire, want, back)
			}
		})
	}
}

// 未知 stop_reason 仍按 end_turn 兜底，不能因为新增档位而改成别的。
func TestAnthropicUnknownStopReasonFallsBack(t *testing.T) {
	if got := anthropic.MapStopReason("brand_new_reason"); got != ir.StopEndTurn {
		t.Errorf("未知值 = %q, want end_turn", got)
	}
}

// ---- 命中的停止串原文 ----

func TestAnthropicCarriesStopSequenceNonStream(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"stop_sequence",` +
		`"stop_sequence":"\n\nHuman:","usage":{"input_tokens":1,"output_tokens":1}}`)
	resp, err := proto.MustOutbound("anthropic").DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.StopReason != ir.StopStopSequence {
		t.Errorf("StopReason = %q, want stop_sequence", resp.StopReason)
	}
	if resp.StopSequence != "\n\nHuman:" {
		t.Errorf("StopSequence = %q, 命中的序列原文丢了", resp.StopSequence)
	}

	out, err := proto.MustInbound("anthropic").EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var got struct {
		StopReason   string `json:"stop_reason"`
		StopSequence string `json:"stop_sequence"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.StopReason != "stop_sequence" || got.StopSequence != "\n\nHuman:" {
		t.Errorf("往返后 = %q / %q\n%s", got.StopReason, got.StopSequence, out)
	}
}

func TestAnthropicCarriesStopSequenceStream(t *testing.T) {
	frames := [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"END"},"usage":{"output_tokens":2}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	d := proto.MustOutbound("anthropic").NewStreamDecoder()
	var delta *ir.Event
	for _, f := range frames {
		evs, err := d.Feed(f[0], f[1])
		if err != nil {
			t.Fatalf("Feed(%q): %v", f[0], err)
		}
		for i := range evs {
			if evs[i].Type == ir.EvMessageDelta {
				delta = &evs[i]
			}
		}
	}
	if delta == nil {
		t.Fatal("没收到 message_delta")
	}
	if delta.StopReason != ir.StopStopSequence {
		t.Errorf("StopReason = %q, want stop_sequence", delta.StopReason)
	}
	if delta.StopSequence != "END" {
		t.Errorf("StopSequence = %q, want END", delta.StopSequence)
	}
}

// 出站编码：命中的序列必须写回 wire，且只在 stop_sequence 档出现。
func TestAnthropicEncodesStopSequenceOnlyWhenMatched(t *testing.T) {
	for _, c := range []struct {
		name   string
		resp   *ir.Response
		expect bool
	}{
		{"matched", &ir.Response{StopReason: ir.StopStopSequence, StopSequence: "END"}, true},
		{"end_turn", &ir.Response{StopReason: ir.StopEndTurn}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			body, err := proto.MustInbound("anthropic").EncodeResponse(c.resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			has := strings.Contains(string(body), "stop_sequence")
			// end_turn 档连键都不该出现：留个 "stop_sequence":"" 会让客户端
			// 以为命中了一条空串。
			if has != c.expect {
				t.Errorf("stop_sequence 出现 = %v, want %v\n%s", has, c.expect, body)
			}
		})
	}
}

// 流式出站也必须把命中的序列写回 wire。此前只测了非流式方向，
// 流式编码器把它丢掉全绿。
func TestAnthropicStreamEncodesStopSequence(t *testing.T) {
	e := proto.MustInbound("anthropic").NewStreamEncoder()
	var all string
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"},
		{Type: ir.EvMessageDelta, StopReason: ir.StopStopSequence, StopSequence: "END",
			Usage: &ir.Usage{OutputTokens: 1}},
		{Type: ir.EvMessageStop},
	} {
		frames, err := e.Encode(ev)
		if err != nil {
			t.Fatalf("Encode(%s): %v", ev.Type, err)
		}
		for _, f := range frames {
			all += string(f)
		}
	}
	if !strings.Contains(all, `"stop_sequence":"END"`) {
		t.Errorf("message_delta 里没有命中的序列原文：\n%s", all)
	}
}

// 反过来：非 stop_sequence 档不能在流里凭空写出一条空序列。
func TestAnthropicStreamOmitsStopSequenceWhenUnmatched(t *testing.T) {
	e := proto.MustInbound("anthropic").NewStreamEncoder()
	frames, err := e.Encode(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn,
		Usage: &ir.Usage{OutputTokens: 1}})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var all string
	for _, f := range frames {
		all += string(f)
	}
	if strings.Contains(all, "stop_sequence") {
		t.Errorf("end_turn 档却出现 stop_sequence：\n%s", all)
	}
}

// ---- pause_turn：目标协议无续跑语义时的降级 ----

// pause_turn 表示「这一轮没做完」。OpenAI 与 Gemini 都没有对应值，
// 但必须映射到「输出不完整」那一档，塌成 stop/STOP 会让客户端把半截结果
// 当成最终答案。
func TestPauseTurnDegradesToIncomplete(t *testing.T) {
	if got := openaichat.UnmapFinishReason(ir.StopPauseTurn); got != "length" {
		t.Errorf("chat finish_reason = %q, want length", got)
	}
	if got := gemini.UnmapFinishReason(ir.StopPauseTurn); got != "MAX_TOKENS" {
		t.Errorf("gemini finishReason = %q, want MAX_TOKENS", got)
	}
}

// stop_sequence 在 OpenAI/Gemini 侧本就没有独立值，收敛到正常结束是正确的
// （与 pause_turn 不同：命中停止串时输出是完整的）。
func TestStopSequenceDegradesToNormalStop(t *testing.T) {
	if got := openaichat.UnmapFinishReason(ir.StopStopSequence); got != "stop" {
		t.Errorf("chat finish_reason = %q, want stop", got)
	}
	if got := gemini.UnmapFinishReason(ir.StopStopSequence); got != "STOP" {
		t.Errorf("gemini finishReason = %q, want STOP", got)
	}
}

// ---- Responses 的 incomplete_details.reason ----

// 风控拦截与输出超长共用 status=incomplete，只看 status 会把拦截报成超长，
// 客户端据此会去加大 max_output_tokens 重试，而真正该做的是改提示词。
func TestResponsesDistinguishesIncompleteReasons(t *testing.T) {
	for _, c := range []struct {
		reason string
		want   ir.StopReason
	}{
		{"content_filter", ir.StopRefusal},
		{"max_output_tokens", ir.StopMaxTokens},
		{"", ir.StopMaxTokens}, // reason 缺失时仍按超长
	} {
		name := c.reason
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			details := ""
			if c.reason != "" {
				details = `,"incomplete_details":{"reason":"` + c.reason + `"}`
			}
			body := []byte(`{"id":"resp_1","model":"m","status":"incomplete","output":[]` + details + `}`)
			resp, err := proto.MustOutbound("openai-responses").DecodeResponse(body)
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			if resp.StopReason != c.want {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, c.want)
			}
		})
	}
}

func TestResponsesStreamDistinguishesIncompleteReasons(t *testing.T) {
	for _, c := range []struct {
		reason string
		want   ir.StopReason
	}{
		{"content_filter", ir.StopRefusal},
		{"max_output_tokens", ir.StopMaxTokens},
	} {
		t.Run(c.reason, func(t *testing.T) {
			frame := `{"type":"response.incomplete","response":{"id":"resp_1","model":"m",` +
				`"status":"incomplete","incomplete_details":{"reason":"` + c.reason + `"}}}`
			d := proto.MustOutbound("openai-responses").NewStreamDecoder()
			evs, err := d.Feed("response.incomplete", frame)
			if err != nil {
				t.Fatalf("Feed: %v", err)
			}
			var got ir.StopReason
			for _, ev := range evs {
				if ev.Type == ir.EvMessageDelta {
					got = ev.StopReason
				}
			}
			if got != c.want {
				t.Errorf("StopReason = %q, want %q", got, c.want)
			}
		})
	}
}

// 出站编码：截断档必须写出 incomplete_details.reason，正常结束档不能写。
func TestResponsesEncodesIncompleteDetails(t *testing.T) {
	for _, c := range []struct {
		stop       ir.StopReason
		wantStatus string
		wantReason string
	}{
		{ir.StopRefusal, "incomplete", "content_filter"},
		{ir.StopMaxTokens, "incomplete", "max_output_tokens"},
		{ir.StopPauseTurn, "incomplete", "max_output_tokens"},
		{ir.StopEndTurn, "completed", ""},
		{ir.StopToolUse, "completed", ""},
		{ir.StopStopSequence, "completed", ""},
	} {
		t.Run(string(c.stop), func(t *testing.T) {
			body, err := proto.MustInbound("openai-responses").EncodeResponse(&ir.Response{
				ID: "resp_1", Model: "m", StopReason: c.stop,
				Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
			})
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			var got struct {
				Status            string `json:"status"`
				IncompleteDetails *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, body)
			}
			if got.Status != c.wantStatus {
				t.Errorf("status = %q, want %q\n%s", got.Status, c.wantStatus, body)
			}
			switch {
			case c.wantReason == "":
				if got.IncompleteDetails != nil {
					t.Errorf("正常结束却写了 incomplete_details：%s", body)
				}
			case got.IncompleteDetails == nil:
				t.Errorf("截断却没写 incomplete_details：%s", body)
			case got.IncompleteDetails.Reason != c.wantReason:
				t.Errorf("reason = %q, want %q", got.IncompleteDetails.Reason, c.wantReason)
			}
		})
	}
}

// 流式出站：截断时事件名也要改成 response.incomplete。此前恒发
// response.completed 只改 status 字段，而解码器（与官方 SDK）按事件名分支，
// completed 分支不读 status —— responses -> responses 往返会把截断吃掉。
func TestResponsesStreamEmitsIncompleteEventName(t *testing.T) {
	for _, c := range []struct {
		stop      ir.StopReason
		wantEvent string
	}{
		{ir.StopRefusal, "response.incomplete"},
		{ir.StopMaxTokens, "response.incomplete"},
		{ir.StopEndTurn, "response.completed"},
	} {
		t.Run(string(c.stop), func(t *testing.T) {
			// 终止帧由 Encode(EvMessageDelta) 就发出（承 R44-E：Responses 的
			// 收尾不在 Finish），所以两边的帧都要收。
			e := proto.MustInbound("openai-responses").NewStreamEncoder()
			var all string
			for _, ev := range []ir.Event{
				{Type: ir.EvMessageStart, MessageID: "resp_1", Model: "m"},
				{Type: ir.EvMessageDelta, StopReason: c.stop, Usage: &ir.Usage{OutputTokens: 1}},
			} {
				frames, err := e.Encode(ev)
				if err != nil {
					t.Fatalf("Encode(%s): %v", ev.Type, err)
				}
				for _, f := range frames {
					all += string(f)
				}
			}
			for _, f := range e.Finish() {
				all += string(f)
			}
			if !strings.Contains(all, c.wantEvent) {
				t.Errorf("没发 %s：\n%s", c.wantEvent, all)
			}
			if c.wantEvent == "response.incomplete" {
				if strings.Contains(all, "response.completed") {
					t.Errorf("截断却发了 response.completed：\n%s", all)
				}
				// 帧体里的 reason 也要在：只对事件名断言的话，把
				// incomplete_details 置空仍然全绿，而客户端正是靠它区分
				// 风控拦截与输出超长。
				want := `"reason":"` + unmapReasonFor(c.stop) + `"`
				if !strings.Contains(all, want) {
					t.Errorf("帧体缺 %s：\n%s", want, all)
				}
			}
		})
	}
}

// unmapReasonFor 测试侧独立复刻一遍档位 -> reason 的期望值。
// 刻意不调用被测的 unmapIncompleteReason（它未导出，且用被测逻辑算期望值
// 等于什么都没断言）。
func unmapReasonFor(s ir.StopReason) string {
	switch s {
	case ir.StopRefusal:
		return "content_filter"
	case ir.StopMaxTokens, ir.StopPauseTurn:
		return "max_output_tokens"
	default:
		return ""
	}
}

// 跨协议端到端：responses 的风控截断经 IR 到 anthropic 客户端必须是 refusal，
// 而不是 max_tokens。
func TestResponsesRefusalReachesAnthropicAsRefusal(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"m","status":"incomplete","output":[],` +
		`"incomplete_details":{"reason":"content_filter"}}`)
	resp, err := proto.MustOutbound("openai-responses").DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, err := proto.MustInbound("anthropic").EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var got struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.StopReason != "refusal" {
		t.Errorf("stop_reason = %q, want refusal（风控被报成了别的）\n%s", got.StopReason, out)
	}
}

// ---- 聚合器 ----

// StopSequence 必须随 message_delta 进 Response，否则缓冲聚合路径
// （上游流式、客户端要非流式）会把它丢掉。
func TestAggregatorKeepsStopSequence(t *testing.T) {
	a := ir.NewAggregator()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg_1", Model: "m"},
		{Type: ir.EvMessageDelta, StopReason: ir.StopStopSequence, StopSequence: "END",
			Usage: &ir.Usage{OutputTokens: 1}},
		{Type: ir.EvMessageStop},
	} {
		a.Feed(ev)
	}
	resp, aerr := a.Finish()
	if aerr != nil {
		t.Fatalf("Finish: %+v", aerr)
	}
	if resp.StopReason != ir.StopStopSequence {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	if resp.StopSequence != "END" {
		t.Errorf("StopSequence = %q, want END", resp.StopSequence)
	}
}
