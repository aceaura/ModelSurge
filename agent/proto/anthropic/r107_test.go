package anthropic

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// R107-甲1 tool_use/server_tool_use 的 caller 回执同族往返：发起方回执原样
// 透传，丢掉等于让上游把回传调用当成来历不明。
func TestToolUseCallerRoundTrip(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":100,` +
		`"tools":[{"name":"get","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],` +
		`"messages":[` +
		`{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_1","name":"get","input":{"q":1},"caller":{"type":"direct"}},` +
		`{"type":"server_tool_use","id":"srvtoolu_2","name":"web_search","input":{"query":"x"},"caller":{"type":"server_tool","tool_use_id":"toolu_1"}}]},` +
		`{"role":"user","content":"ok"}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var tuCaller, stuCaller string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolUse != nil {
				tuCaller = string(b.ToolUse.Caller)
			}
			if b.Type == ir.BlockServerToolUse && b.ServerToolUse != nil {
				stuCaller = string(b.ServerToolUse.Caller)
			}
		}
	}
	if tuCaller != `{"type":"direct"}` {
		t.Fatalf("tool_use caller = %s", tuCaller)
	}
	if stuCaller != `{"type":"server_tool","tool_use_id":"toolu_1"}` {
		t.Fatalf("server_tool_use caller = %s", stuCaller)
	}

	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"caller":{"type":"direct"}`, `"caller":{"type":"server_tool","tool_use_id":"toolu_1"}`} {
		if !strings.Contains(s, want) {
			t.Errorf("re-encoded request missing %s: %s", want, s)
		}
	}

	// 对照组：没给 caller 的调用不得发明该键（同样声明工具，避免降级干扰）。
	plain, err := New().DecodeRequest([]byte(
		`{"model":"claude-x","max_tokens":100,"tools":[{"name":"get","input_schema":{"type":"object"}}],` +
			`"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get","input":{}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	pout, err := New().EncodeRequest(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pout), `"caller"`) {
		t.Errorf("absent caller must not be invented: %s", pout)
	}
}

// R107-乙1/乙2/乙3 beta 请求参数 context_management / diagnostics /
// user_profile_id 同族往返。
func TestBetaParamsRoundTrip(t *testing.T) {
	body := `{"model":"claude-x","max_tokens":100,"user_profile_id":"profile_9",` +
		`"context_management":{"edits":[{"type":"clear_tool_uses_20250919","keep":{"type":"tool_uses","value":3}}]},` +
		`"diagnostics":{"previous_message_id":"msg_prev"},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	req, err := New().DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.AnthropicContextMgmt) == 0 || len(req.AnthropicDiagnostics) == 0 || req.UserProfileID != "profile_9" {
		t.Fatalf("beta params dropped at decode: ctx=%s diag=%s profile=%q",
			req.AnthropicContextMgmt, req.AnthropicDiagnostics, req.UserProfileID)
	}

	out, err := New().EncodeRequest(req.Clone())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"user_profile_id":"profile_9"`,
		`"context_management":{"edits":[{"type":"clear_tool_uses_20250919","keep":{"type":"tool_uses","value":3}}]}`,
		`"diagnostics":{"previous_message_id":"msg_prev"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("re-encoded request missing %s: %s", want, s)
		}
	}

	// 对照组：没给这些参数的请求不得发明键。
	plain, err := New().DecodeRequest([]byte(
		`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	pout, err := New().EncodeRequest(plain)
	if err != nil {
		t.Fatal(err)
	}
	ps := string(pout)
	for _, key := range []string{`"user_profile_id"`, `"context_management"`, `"diagnostics"`} {
		if strings.Contains(ps, key) {
			t.Errorf("absent param %s must not be invented: %s", key, ps)
		}
	}
}

// R107-乙7 beta 非流式响应的 context_management / diagnostics 回执同族往返。
func TestBetaResponseReceiptsRoundTrip(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":1},` +
		`"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":2}]},` +
		`"diagnostics":{"cache_miss_reason":"system_prompt_changed"}}`
	resp, err := New().DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.AnthropicContextMgmt) == 0 || len(resp.AnthropicDiagnostics) == 0 {
		t.Fatalf("response receipts dropped at decode: ctx=%s diag=%s",
			resp.AnthropicContextMgmt, resp.AnthropicDiagnostics)
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":2}]}`,
		`"diagnostics":{"cache_miss_reason":"system_prompt_changed"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("re-encoded response missing %s: %s", want, s)
		}
	}

	// 对照组：无回执的响应不得发明键。
	plain, err := New().DecodeResponse([]byte(
		`{"id":"msg_2","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	pout, err := New().EncodeResponse(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pout), `"context_management"`) || strings.Contains(string(pout), `"diagnostics"`) {
		t.Errorf("absent receipts must not be invented: %s", pout)
	}
}

// R107-甲7 未知事件型/delta 型计数注记：不再静默丢弃。
func TestUnknownStreamFramesCounted(t *testing.T) {
	dec := New().NewStreamDecoder()
	for _, frame := range [][2]string{
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"frobnicate_delta","x":1}}`},
		{"frobnicate_event", `{"type":"frobnicate_event","data":{}}`},
	} {
		if _, err := dec.Feed(frame[0], frame[1]); err != nil {
			t.Fatal(err)
		}
	}
	notes := dec.(interface{ Notes() []string }).Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "2 stream event(s) or delta(s)") {
		t.Fatalf("未知帧计数注记不对：%q", notes)
	}
	if again := dec.(interface{ Notes() []string }).Notes(); len(again) != 0 {
		t.Errorf("Notes() 未排干：%q", again)
	}
}

// 对照组：已知事件型走完一轮零注记（原生形态协议必须闭嘴）。
func TestKnownStreamFramesLeaveNoNotes(t *testing.T) {
	dec := New().NewStreamDecoder()
	for _, frame := range [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		if _, err := dec.Feed(frame[0], frame[1]); err != nil {
			t.Fatal(err)
		}
	}
	if notes := dec.(interface{ Notes() []string }).Notes(); len(notes) != 0 {
		t.Errorf("已知帧全程不该有注记：%q", notes)
	}
}

// R107-甲5 message_delta 只写官方六键：明细与地理回显由 message_start 承担，
// delta 帧写 cache_creation 对象是官方 MessageDeltaUsage 没有的键。
func TestMessageDeltaWritesOnlyOfficialUsageKeys(t *testing.T) {
	start := ir.Usage{InputTokens: 100, CacheCreationTokens: 30, CacheCreation5mTokens: 20,
		CacheCreation1hTokens: 10, CacheCreationDetailsKnown: true, InferenceGeo: "us"}
	delta := ir.Usage{OutputTokens: 5}
	enc := New().NewStreamEncoder()
	var out []byte
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "claude", Usage: &start},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &delta},
		{Type: ir.EvMessageStop},
	} {
		frames, err := enc.Encode(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			out = append(out, f...)
		}
	}
	s := string(out)
	di := strings.Index(s, `"message_delta"`)
	if di < 0 {
		t.Fatalf("缺 message_delta 帧：\n%s", s)
	}
	deltaFrame := s[di:]
	if strings.Contains(deltaFrame, "ephemeral") || strings.Contains(deltaFrame, "inference_geo") || strings.Contains(deltaFrame, `"cache_creation"`) {
		t.Errorf("message_delta 写了官方没有的键：\n%s", deltaFrame)
	}
	// 明细只能在 message_start 里。
	startFrame := s[:di]
	if !strings.Contains(startFrame, `"ephemeral_5m_input_tokens":20`) || !strings.Contains(startFrame, `"inference_geo":"us"`) {
		t.Errorf("message_start 丢了明细/地理回显：\n%s", startFrame)
	}
}
