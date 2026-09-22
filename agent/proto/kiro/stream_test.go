// stream_test.go 流式解码 golden 测试
// （KiroaaS tests/unit/test_parsers.py / test_thinking_parser.py /
// test_streaming_core.py 代表样例的 Go 移植：事件字节 -> IR 事件序列断言）。
package kiro

import (
	"io"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// feedBytes 把原始字节喂给事件流解析器，返回提取出的 (kind, data) 列表。
func feedBytes(t *testing.T, chunks ...string) []kiroRawEvent {
	t.Helper()
	p := eventStreamParser{}
	var out []kiroRawEvent
	for _, c := range chunks {
		out = append(out, p.feed([]byte(c))...)
	}
	return out
}

func kinds(evs []kiroRawEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.kind
	}
	return out
}

// ---- eventStreamParser：事件提取（test_parsers.py 移植） ----

func TestParser_ContentEvent(t *testing.T) {
	evs := feedBytes(t, `{"content":"Hello World"}`)
	if len(evs) != 1 || evs[0].kind != "content" || !strings.Contains(evs[0].data, "Hello World") {
		t.Fatalf("got %+v", evs)
	}
}

func TestParser_MultipleContentEvents(t *testing.T) {
	evs := feedBytes(t, `{"content":"First"}{"content":"Second"}`)
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if !strings.Contains(evs[0].data, "First") || !strings.Contains(evs[1].data, "Second") {
		t.Fatalf("order wrong: %+v", evs)
	}
}

func TestParser_DeduplicatesRepeatedContent(t *testing.T) {
	evs := feedBytes(t, `{"content":"Same"}`, `{"content":"Same"}`)
	if len(evs) != 1 {
		t.Fatalf("duplicate not filtered: %+v", evs)
	}
}

func TestParser_UsageEventNumeric(t *testing.T) {
	evs := feedBytes(t, `{"usage":1.5}`)
	if len(evs) != 1 || evs[0].kind != "usage" {
		t.Fatalf("got %+v", evs)
	}
}

func TestParser_ContextUsageEvent(t *testing.T) {
	evs := feedBytes(t, `{"contextUsagePercentage":25.5}`)
	if len(evs) != 1 || evs[0].kind != "context_usage" {
		t.Fatalf("got %+v", evs)
	}
}

func TestParser_CompletesJSONAcrossChunks(t *testing.T) {
	evs := feedBytes(t, `{"content":"He`, `llo"}`)
	if len(evs) != 1 || !strings.Contains(evs[0].data, "Hello") {
		t.Fatalf("got %+v", evs)
	}
}

func TestParser_IncompleteJSONBuffered(t *testing.T) {
	evs := feedBytes(t, `{"content":"He`)
	if len(evs) != 0 {
		t.Fatalf("incomplete JSON should buffer, got %+v", evs)
	}
}

func TestParser_GarbageBetweenEvents(t *testing.T) {
	evs := feedBytes(t, "\x00\x01binary noise\x00"+`{"content":"A"}`+"\x00\x02more noise"+`{"content":"B"}`)
	if len(evs) != 2 {
		t.Fatalf("noise should be skipped: %+v", evs)
	}
}

func TestParser_FollowupSkipped(t *testing.T) {
	evs := feedBytes(t, `{"followupPrompt":{"reason":"test"}}`)
	if len(evs) != 0 {
		t.Fatalf("followup should be skipped: %+v", evs)
	}
}

func TestParser_EscapedQuotesInContent(t *testing.T) {
	evs := feedBytes(t, `{"content":"say \"hi\" {ok}"}`)
	if len(evs) != 1 || !strings.Contains(evs[0].data, `{ok}`) {
		t.Fatalf("got %+v", evs)
	}
}

// ---- thinkingParser FSM（test_thinking_parser.py 移植） ----

func TestThinking_DetectsAllOpenTags(t *testing.T) {
	for _, tag := range thinkingOpenTags {
		tp := newThinkingParser()
		tp.feed(tag + "Hello")
		if tp.state != inThinking || tp.openTag != tag {
			t.Fatalf("tag %q: state=%v openTag=%q", tag, tp.state, tp.openTag)
		}
		if tp.closeTag != "</"+tag[1:] {
			t.Fatalf("tag %q: closeTag=%q", tag, tp.closeTag)
		}
	}
}

func TestThinking_StripsLeadingWhitespace(t *testing.T) {
	tp := newThinkingParser()
	tp.feed("  \n\n<thinking>Hello")
	if tp.state != inThinking || tp.openTag != "<thinking>" {
		t.Fatalf("state=%v openTag=%q", tp.state, tp.openTag)
	}
}

func TestThinking_BuffersPartialTag(t *testing.T) {
	tp := newThinkingParser()
	res := tp.feed("<think")
	if tp.state != preContent || tp.initialBuffer != "<think" || res.stateChanged {
		t.Fatalf("state=%v buffer=%q", tp.state, tp.initialBuffer)
	}
}

func TestThinking_CompletesPartialTag(t *testing.T) {
	tp := newThinkingParser()
	tp.feed("<think")
	tp.feed("ing>Hello")
	if tp.state != inThinking || tp.openTag != "<thinking>" {
		t.Fatalf("state=%v openTag=%q", tp.state, tp.openTag)
	}
}

func TestThinking_NoTagTransitionsToStreaming(t *testing.T) {
	tp := newThinkingParser()
	text := "Hello, this is regular content without any thinking tags."
	res := tp.feed(text)
	if tp.state != streaming || res.regularContent != text {
		t.Fatalf("state=%v regular=%q", tp.state, res.regularContent)
	}
}

func TestThinking_BufferExceedsLimitTransitionsToStreaming(t *testing.T) {
	tp := newThinkingParser()
	res := tp.feed("This is way more than twenty characters of ordinary text")
	if tp.state != streaming {
		t.Fatalf("state=%v", tp.state)
	}
	if res.regularContent == "" {
		t.Fatal("buffered content should flush as regular content")
	}
}

func TestThinking_ClosingTagTransitionsToStreaming(t *testing.T) {
	tp := newThinkingParser()
	tp.feed("<thinking>Hello")
	res := tp.feed("</thinking>World")
	if tp.state != streaming {
		t.Fatalf("state=%v", tp.state)
	}
	if res.thinkingContent != "Hello" || !res.isLastThinking {
		t.Fatalf("thinking=%q isLast=%v", res.thinkingContent, res.isLastThinking)
	}
	if res.regularContent != "World" {
		t.Fatalf("regular=%q", res.regularContent)
	}
}

func TestThinking_CautiousBufferingKeepsTail(t *testing.T) {
	tp := newThinkingParser()
	tp.feed("<thinking>")
	tp.feed(strings.Repeat("A", 50))
	if len(tp.thinkingBuffer) > tp.maxTagLength {
		t.Fatalf("buffer should keep at most maxTagLength, got %d", len(tp.thinkingBuffer))
	}
}

func TestThinking_SplitClosingTagDetected(t *testing.T) {
	tp := newThinkingParser()
	tp.feed("<thinking>Hello")
	tp.feed("</think")
	res := tp.feed("ing>World")
	if tp.state != streaming {
		t.Fatalf("split close tag not detected, state=%v", tp.state)
	}
	if res.regularContent != "World" {
		t.Fatalf("regular=%q", res.regularContent)
	}
}

func TestThinking_IgnoresTagsInStreamingState(t *testing.T) {
	tp := newThinkingParser()
	tp.feed("Regular content")
	res := tp.feed("<thinking>This should be regular</thinking>")
	if res.regularContent != "<thinking>This should be regular</thinking>" {
		t.Fatalf("regular=%q", res.regularContent)
	}
	if res.thinkingContent != "" {
		t.Fatalf("unexpected thinking=%q", res.thinkingContent)
	}
}

func TestThinking_FinalizeFlushesBuffers(t *testing.T) {
	tp := newThinkingParser()
	tp.feed("<thinking>Incomplete thinking")
	res := tp.finalize()
	if res.thinkingContent != "Incomplete thinking" || !res.isLastThinking {
		t.Fatalf("thinking=%q isLast=%v", res.thinkingContent, res.isLastThinking)
	}

	tp2 := newThinkingParser()
	tp2.feed("<thi")
	res2 := tp2.finalize()
	if res2.regularContent != "<thi" {
		t.Fatalf("regular=%q", res2.regularContent)
	}
}

// ---- 工具去重与截断诊断（test_parsers.py 移植） ----

func TestDedupToolCalls_ByIDKeepsBetterArgs(t *testing.T) {
	calls := []kiroToolCall{
		{ID: "call_1", Name: "func", args: "{}"},
		{ID: "call_1", Name: "func", args: `{"key":"value"}`},
	}
	got := dedupToolCalls(calls)
	if len(got) != 1 || got[0].args != `{"key":"value"}` {
		t.Fatalf("got %+v", got)
	}
}

func TestDedupToolCalls_InvalidReplacedByValid(t *testing.T) {
	calls := []kiroToolCall{
		{ID: "call_1", Name: "func", args: `{"a":`, invalid: true},
		{ID: "call_1", Name: "func", args: `{"a":1}`},
	}
	got := dedupToolCalls(calls)
	if len(got) != 1 || got[0].invalid || got[0].args != `{"a":1}` {
		t.Fatalf("got %+v", got)
	}
}

func TestDedupToolCalls_InvalidKeepsLongerRaw(t *testing.T) {
	calls := []kiroToolCall{
		{ID: "call_1", Name: "func", args: `{"a":`, invalid: true, truncated: true},
		{ID: "call_1", Name: "func", args: `{"a":"longer`, invalid: true, truncated: true},
	}
	got := dedupToolCalls(calls)
	if len(got) != 1 || got[0].args != `{"a":"longer` {
		t.Fatalf("got %+v", got)
	}
}

func TestDedupToolCalls_DistinctMalformedNotCollapsed(t *testing.T) {
	calls := []kiroToolCall{
		{ID: "call_1", Name: "func", args: `{"a":`, invalid: true},
		{ID: "call_2", Name: "func", args: `{"b":`, invalid: true},
	}
	if got := dedupToolCalls(calls); len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestDedupToolCalls_RemovesNameArgsDuplicates(t *testing.T) {
	calls := []kiroToolCall{
		{ID: "call_1", Name: "func", args: `{"a":1}`},
		{ID: "call_2", Name: "func", args: `{"a":1}`},
	}
	got := dedupToolCalls(calls)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestDedupToolCalls_MixedWithAndWithoutID(t *testing.T) {
	calls := []kiroToolCall{
		{ID: "call_1", Name: "func1", args: `{"a":1}`},
		{Name: "func2", args: `{"b":2}`},
		{ID: "call_3", Name: "func3", args: `{"c":3}`},
	}
	got := dedupToolCalls(calls)
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestDiagnoseJSONTruncation_Issue34(t *testing.T) {
	// KiroaaS Issue #34 的真实样例：JSON 在 filePath 值后被截断
	s := `{"filePath": "/Users/cc/Documents/Code/mock-all/docs/plans/2026-01-12-mock-all-impl.md"`
	truncated, reason := diagnoseJSONTruncation(s)
	if !truncated || !strings.Contains(reason, "brace") {
		t.Fatalf("truncated=%v reason=%q", truncated, reason)
	}
}

func TestDiagnoseJSONTruncation_ValidJSON(t *testing.T) {
	for _, s := range []string{``, `   `, `{"a":1}`, `{"a":{"b":[1,2]}}`} {
		if truncated, _ := diagnoseJSONTruncation(s); truncated {
			t.Fatalf("%q should not be truncated", s)
		}
	}
}

func TestDiagnoseJSONTruncation_UnclosedString(t *testing.T) {
	// 括号平衡但字符串未闭合：{"a":"un_closed...} —— "a" 2 个引号 + 未闭合 1 个 = 奇数
	truncated, reason := diagnoseJSONTruncation(`{"a":"un_closed_string_here}`)
	if !truncated || !strings.Contains(reason, "string") {
		t.Fatalf("truncated=%v reason=%q", truncated, reason)
	}
}

func TestToolCallFinalize_MalformedPreservesRaw(t *testing.T) {
	tc := kiroToolCall{args: `{"a":,}`}
	tc.finalize()
	if !tc.invalid || tc.truncated || tc.args != `{"a":,}` || tc.truncReason != "malformed JSON" {
		t.Fatalf("got %+v", tc)
	}
}

// ---- streamDecoder：事件序列 -> IR 事件 ----

// driveDecoder 用原始字节块驱动完整解码管线，返回全部 IR 事件。
func driveDecoder(t *testing.T, chunks ...string) []ir.Event {
	t.Helper()
	dec := &streamDecoder{}
	p := eventStreamParser{}
	var out []ir.Event
	for _, c := range chunks {
		for _, raw := range p.feed([]byte(c)) {
			evs, err := dec.Feed("", raw.data)
			if err != nil {
				t.Fatalf("feed %s: %v", raw.data, err)
			}
			out = append(out, evs...)
		}
	}
	return append(out, dec.Finish()...)
}

// summary 把事件序列压缩为可断言的摘要串列表。
func summary(evs []ir.Event) []string {
	var out []string
	for _, ev := range evs {
		switch ev.Type {
		case ir.EvMessageStart:
			out = append(out, "start")
		case ir.EvBlockStart:
			out = append(out, "block_start:"+string(ev.Block.Type))
		case ir.EvTextDelta:
			out = append(out, "text:"+ev.Text)
		case ir.EvThinkingDelta:
			out = append(out, "think:"+ev.Text)
		case ir.EvSigDelta:
			out = append(out, "sig")
		case ir.EvToolInput:
			out = append(out, "tool_input:"+ev.Text)
		case ir.EvBlockStop:
			out = append(out, "block_stop")
		case ir.EvMessageDelta:
			out = append(out, "delta:"+string(ev.StopReason))
		case ir.EvMessageStop:
			out = append(out, "stop")
		default:
			out = append(out, string(ev.Type))
		}
	}
	return out
}

func wantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count: got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d]: got %q want %q\nfull got: %v", i, got[i], want[i], got)
		}
	}
}

func TestDecoder_PlainContentStream(t *testing.T) {
	evs := summary(driveDecoder(t,
		`{"content":"Hello "}`, `{"content":"World"}`, `{"contextUsagePercentage":50.0}`,
	))
	wantEvents(t, evs,
		"start",
		"block_start:text", "text:Hello ", "text:World", "block_stop",
		"delta:end_turn", "stop",
	)
}

func TestDecoder_TruncatedContentMaxTokens(t *testing.T) {
	// 无完成信号（缺 context_usage/usage）且有正文 -> 截断 -> max_tokens
	evs := summary(driveDecoder(t, `{"content":"partial answer..."}`))
	for _, s := range evs {
		if s == "delta:max_tokens" {
			return
		}
	}
	t.Fatalf("want max_tokens stop reason, got %v", evs)
}

func TestDecoder_ToolCallChain(t *testing.T) {
	evs := summary(driveDecoder(t,
		`{"content":"Let me check."}`,
		`{"name":"get_weather","toolUseId":"call_123"}`,
		`{"input":"{\"city\":"}`,
		`{"input":"\"Paris\"}"}`,
		`{"stop":true}`,
		`{"contextUsagePercentage":10.0}`,
	))
	wantEvents(t, evs,
		"start",
		"block_start:text", "text:Let me check.", "block_stop",
		"block_start:tool_use",
		"tool_input:"+`{"city":"Paris"}`,
		"block_stop",
		"delta:tool_use", "stop",
	)
}

func TestDecoder_TruncatedToolArgsPreserved(t *testing.T) {
	dec := &streamDecoder{}
	for _, raw := range []string{
		`{"name":"get_weather","toolUseId":"call_bad"}`,
		`{"input":"{\"city\":\"Par"}`,
		`{"stop":true}`,
	} {
		if _, err := dec.Feed("", raw); err != nil {
			t.Fatal(err)
		}
	}
	evs := dec.Finish()
	found := false
	for _, ev := range evs {
		if ev.Type == ir.EvToolInput {
			found = true
			if ev.Text != `{"city":"Par` {
				t.Fatalf("tool input = %q", ev.Text)
			}
		}
	}
	if !found {
		t.Fatal("no tool input event")
	}
	truncated := dec.TruncatedTools()
	if len(truncated) != 1 || truncated[0].ID != "call_bad" {
		t.Fatalf("truncated = %+v", truncated)
	}
}

// 真实上游（gpt 系）toolUseEvent 每帧回显 name+toolUseId（2026-09-15
// kiro-2/gpt-5.6-sol debug 抓包）：续片/结束帧先判 name 会碎成多段、
// 只剩恰好自解析的片段（曾表现为 arguments="\":\""）。
func TestDecoder_ToolCallNameEchoEveryFrame(t *testing.T) {
	evs := summary(driveDecoder(t,
		`{"name":"get_weather","toolUseId":"call_4066e4ff"}`,
		`{"input":"{\"","name":"get_weather","toolUseId":"call_4066e4ff"}`,
		`{"input":"city","name":"get_weather","toolUseId":"call_4066e4ff"}`,
		`{"input":"\":\"","name":"get_weather","toolUseId":"call_4066e4ff"}`,
		`{"input":"Paris","name":"get_weather","toolUseId":"call_4066e4ff"}`,
		`{"input":"\"}","name":"get_weather","toolUseId":"call_4066e4ff"}`,
		`{"name":"get_weather","stop":true,"toolUseId":"call_4066e4ff"}`,
		`{"stopReason":"END_TURN"}`,
		`{"contextUsagePercentage":0.172}`,
		`{"unit":"credit","unitPlural":"credits","usage":0.021}`,
	))
	wantEvents(t, evs,
		"start",
		"block_start:tool_use",
		"tool_input:"+`{"city":"Paris"}`,
		"block_stop",
		"delta:tool_use", "stop",
	)
}

// 回显形态下并行两调用：第二调用的 start 帧开启新调用并携带首片，
// 迟到的 stop 不误关当前调用。
func TestDecoder_ToolCallNameEchoParallel(t *testing.T) {
	evs := summary(driveDecoder(t,
		`{"name":"read_file","toolUseId":"call_a"}`,
		`{"input":"{\"path\"","name":"read_file","toolUseId":"call_a"}`,
		`{"input":":\"a.txt\"}","name":"read_file","toolUseId":"call_a"}`,
		`{"name":"read_file","toolUseId":"call_b"}`,
		`{"input":"{\"path\"","name":"read_file","toolUseId":"call_b"}`,
		`{"input":":\"b.txt\"}","name":"read_file","toolUseId":"call_b"}`,
		`{"name":"read_file","stop":true,"toolUseId":"call_b"}`,
		`{"name":"read_file","stop":true,"toolUseId":"call_a"}`,
		`{"contextUsagePercentage":1.0}`,
	))
	wantEvents(t, evs,
		"start",
		"block_start:tool_use", "tool_input:"+`{"path":"a.txt"}`, "block_stop",
		"block_start:tool_use", "tool_input:"+`{"path":"b.txt"}`, "block_stop",
		"delta:tool_use", "stop",
	)
}

// 单帧完成形态：name+input+stop 同帧（对象参数直出）。
func TestDecoder_ToolCallSingleFrameComplete(t *testing.T) {
	evs := summary(driveDecoder(t,
		`{"name":"lookup","toolUseId":"call_s","input":{"key":"v"},"stop":true}`,
		`{"contextUsagePercentage":0.5}`,
	))
	wantEvents(t, evs,
		"start",
		"block_start:tool_use",
		"tool_input:"+`{"key":"v"}`,
		"block_stop",
		"delta:tool_use", "stop",
	)
}

// stop 非真值帧（false/{}）不收尾、不误开调用。
func TestDecoder_ToolCallStopFalsyIgnored(t *testing.T) {
	evs := summary(driveDecoder(t,
		`{"name":"get_weather","toolUseId":"call_f"}`,
		`{"stop":false}`,
		`{"stop":{}}`,
		`{"input":"{}","name":"get_weather","toolUseId":"call_f"}`,
		`{"stop":true}`,
		`{"contextUsagePercentage":0.0}`,
	))
	wantEvents(t, evs,
		"start",
		"block_start:tool_use",
		"tool_input:"+`{}`,
		"block_stop",
		"delta:tool_use", "stop",
	)
}

func TestDecoder_ToolNameAliasReversed(t *testing.T) {
	ResetToolAliases()
	defer ResetToolAliases()
	longName := "very_long_tool_name_exceeding_sixty_four_characters_limit_padding_pad_" + strings.Repeat("x", 10)
	alias := AliasForToolName(longName)
	evs := driveDecoder(t,
		`{"name":"`+alias+`","toolUseId":"call_1"}`,
		`{"stop":true}`,
		`{"contextUsagePercentage":5.0}`,
	)
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block.Type == ir.BlockToolUse {
			if ev.Block.ToolUse.Name != longName {
				t.Fatalf("alias %q not reversed, got %q", alias, ev.Block.ToolUse.Name)
			}
			return
		}
	}
	t.Fatal("no tool_use block emitted")
}

func TestDecoder_NativeThinkingWithSignature(t *testing.T) {
	evs := summary(driveDecoder(t,
		`{"text":"pondering deeply"}`,
		`{"signature":"real_signature_value"}`,
		`{"content":"Answer"}`,
		`{"contextUsagePercentage":20.0}`,
	))
	wantEvents(t, evs,
		"start",
		"block_start:thinking", "think:pondering deeply", "sig", "block_stop",
		"block_start:text", "text:Answer", "block_stop",
		"delta:end_turn", "stop",
	)
}

func TestDecoder_FakeReasoningTagExtraction(t *testing.T) {
	old := opt
	opt = Options{FakeReasoning: true, FakeReasoningMaxTokens: 4000, FakeReasoningBudgetCap: 10000}
	defer func() { opt = old }()

	evs := summary(driveDecoder(t,
		`{"content":"<thinking>secret "}`,
		`{"content":"thoughts</thinking>Hello"}`,
		`{"contextUsagePercentage":30.0}`,
	))
	wantEvents(t, evs,
		"start",
		"block_start:thinking", "think:secret thoughts", "sig", "block_stop",
		"block_start:text", "text:Hello", "block_stop",
		"delta:end_turn", "stop",
	)
}

// 账号级 fake_reasoning：全局关 + SetFakeReasoning(true) 时标签解析照常生效
// （relay 经 newDecoder 注入缝挂载）。
func TestDecoder_FakeReasoningAccountLevel(t *testing.T) {
	old := opt
	opt = Options{}
	defer func() { opt = old }()

	dec := &streamDecoder{}
	dec.SetFakeReasoning(true)
	p := eventStreamParser{}
	var evs []ir.Event
	for _, raw := range p.feed([]byte(
		`{"content":"<thinking>secret "}` +
			`{"content":"thoughts</thinking>Hello"}` +
			`{"contextUsagePercentage":30.0}`,
	)) {
		out, err := dec.Feed("", raw.data)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, out...)
	}
	evs = append(evs, dec.Finish()...)
	wantEvents(t, summary(evs),
		"start",
		"block_start:thinking", "think:secret thoughts", "sig", "block_stop",
		"block_start:text", "text:Hello", "block_stop",
		"delta:end_turn", "stop",
	)
}

func TestDecoder_UsageCacheFields(t *testing.T) {
	dec := &streamDecoder{}
	p := eventStreamParser{}
	var evs []ir.Event
	for _, raw := range p.feed([]byte(
		`{"content":"` + strings.Repeat("hello ", 100) + `"}` +
			`{"usage":{"cache_read_input_tokens":100,"cacheCreationInputTokens":50}}` +
			`{"contextUsagePercentage":25.0}`,
	)) {
		out, err := dec.Feed("", raw.data)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, out...)
	}
	evs = append(evs, dec.Finish()...)
	var usage *ir.Usage
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			usage = ev.Usage
		}
	}
	if usage == nil {
		t.Fatal("no message_delta usage")
	}
	if usage.CacheReadTokens != 100 || usage.CacheCreationTokens != 50 {
		t.Fatalf("cache fields: %+v", usage)
	}
	// context_usage=25% × 默认 200000 = 50000 总量，减输出估算
	if usage.InputTokens <= 0 || usage.InputTokens > 50000 {
		t.Fatalf("input tokens from context usage: %+v", usage)
	}
	if usage.OutputTokens == 0 {
		t.Fatalf("output should be estimated: %+v", usage)
	}
}

func TestDecoder_BracketToolCallsExtracted(t *testing.T) {
	evs := summary(driveDecoder(t,
		`{"content":"[Called get_time with args: {\"timezone\":\"UTC\"}]"}`,
		`{"contextUsagePercentage":15.0}`,
	))
	found := false
	for _, s := range evs {
		if s == "delta:tool_use" {
			found = true
		}
		if strings.HasPrefix(s, "tool_input:") && strings.Contains(s, "timezone") {
			return
		}
	}
	t.Fatalf("bracket tool call not extracted: %v (stop_reason tool_use=%v)", evs, found)
}

// 空流：上游一个字节都没说就关了。骨架仍要发全（下游编码器依赖成对事件），
// 但停止原因必须是中断档——报 end_turn 会让客户端把空回答当成最终答复。
func TestDecoder_EmptyStreamProducesSkeleton(t *testing.T) {
	evs := summary(driveDecoder(t))
	wantEvents(t, evs, "start", "delta:aborted", "stop")
}

func TestDecoder_FinishIdempotent(t *testing.T) {
	dec := &streamDecoder{}
	dec.Feed("", `{"content":"hi"}`)
	first := dec.Finish()
	if len(first) == 0 {
		t.Fatal("first Finish should produce events")
	}
	if again := dec.Finish(); len(again) != 0 {
		t.Fatalf("second Finish should be empty, got %v", summary(again))
	}
}

func TestDecoder_DuplicateToolCallsDeduped(t *testing.T) {
	// 同名同参数（不同 ID）-> 名字+参数维度去重为一次
	evs := driveDecoder(t,
		`{"name":"func","toolUseId":"call_1"}`,
		`{"input":"{\"key\":\"value\"}"}`,
		`{"stop":true}`,
		`{"name":"func","toolUseId":"call_2"}`,
		`{"input":"{\"key\":\"value\"}"}`,
		`{"stop":true}`,
		`{"contextUsagePercentage":10.0}`,
	)
	var toolStarts int
	for _, ev := range evs {
		if ev.Type == ir.EvBlockStart && ev.Block.Type == ir.BlockToolUse {
			toolStarts++
		}
	}
	if toolStarts != 1 {
		t.Fatalf("want 1 deduped tool block, got %d", toolStarts)
	}
}

// ---- EventStreamToSSE 适配器 ----

func TestEventStreamToSSE(t *testing.T) {
	src := strings.NewReader(
		"\x00noise\x00" + `{"content":"A"}` + "\x01" + `{"content":"B"}`,
	)
	r := NewEventStreamToSSE(src)
	var out []byte
	buf := make([]byte, 7) // 故意小块读，验证分帧
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	s := string(out)
	want := "data: {\"content\":\"A\"}\n\ndata: {\"content\":\"B\"}\n\n"
	if s != want {
		t.Fatalf("got %q want %q", s, want)
	}
}

// ---- DecodeResponse / EncodeResponse 往返 ----

func TestResponseRoundTrip(t *testing.T) {
	resp := &ir.Response{
		ID:    "msg_test",
		Model: "claude-sonnet-4.6",
		Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm", Signature: "sig_x"}},
			{Type: ir.BlockText, Text: "Hello"},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "get_weather", Input: []byte(`{"city":"Paris"}`)}},
		},
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 100},
	}
	body, err := Codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟上游一次性返回：事件之间夹二进制噪声
	noisy := append([]byte("\x00\x01\x02"), body...)
	decoded, err := Codec{}.DecodeResponse(noisy)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.StopReason != ir.StopToolUse {
		t.Fatalf("stop reason: %v", decoded.StopReason)
	}
	if len(decoded.Content) != 3 {
		t.Fatalf("blocks: %d", len(decoded.Content))
	}
	if b := decoded.Content[0]; b.Type != ir.BlockThinking || b.Thinking.Text != "hmm" || b.Thinking.Signature != "sig_x" {
		t.Fatalf("thinking block: %+v", b)
	}
	if b := decoded.Content[1]; b.Type != ir.BlockText || b.Text != "Hello" {
		t.Fatalf("text block: %+v", b)
	}
	if b := decoded.Content[2]; b.Type != ir.BlockToolUse || b.ToolUse.Name != "get_weather" {
		t.Fatalf("tool block: %+v", b)
	}
	if string(decoded.Content[2].ToolUse.Input) != `{"city":"Paris"}` {
		t.Fatalf("tool input: %s", decoded.Content[2].ToolUse.Input)
	}
	if decoded.Usage.CacheReadTokens != 100 {
		t.Fatalf("cache usage: %+v", decoded.Usage)
	}
}

func TestEncodeResponse_TruncatedOmitsContextUsage(t *testing.T) {
	resp := &ir.Response{
		Content:    []ir.Block{{Type: ir.BlockText, Text: "cut off"}},
		StopReason: ir.StopMaxTokens,
	}
	body, err := Codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "contextUsagePercentage") {
		t.Fatalf("truncated response must not emit completion signal: %s", body)
	}
	decoded, err := Codec{}.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.StopReason != ir.StopMaxTokens {
		t.Fatalf("round trip lost max_tokens: %v", decoded.StopReason)
	}
}
