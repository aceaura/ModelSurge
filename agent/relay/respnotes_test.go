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
	"github.com/aceaura/ModelSurge/replay/contract/replayv1"
)

// R55 响应侧损耗的投递通道：非流式并入 X-ModelSurge-Notes 响应头，
// 流式（头已发出）落 SSE 注释帧 + 日志。两条路都只报不拒。

func r55LossyResp() *ir.Response {
	return &ir.Response{
		ID: "m1", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "t", Signature: "sig-x", SignatureFrom: "gemini"}},
			{Type: ir.BlockText, Text: "hi"},
		},
	}
}

// 非流式：编码前扫描的损耗注记进响应头；请求侧已写的注记要保住（"; " 合并）。
func TestWriteResponseNotesIntoHeader(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-ModelSurge-Notes", "request-side note")
	writeResponse(w, proto.MustInbound("anthropic"), nil, r55LossyResp(), false, nil)
	h := w.Header().Get("X-ModelSurge-Notes")
	if !strings.Contains(h, "request-side note") {
		t.Errorf("请求侧注记被覆盖：%q", h)
	}
	if !strings.Contains(h, "dropped 1 thought signature(s)") {
		t.Errorf("响应侧签名损耗未进头：%q", h)
	}
	if !strings.Contains(h, "; ") {
		t.Errorf("两侧注记未用分隔符合并：%q", h)
	}
}

// 聚合期挪键注记（respNotes 参数）与编码扫描注记要一起进头。
func TestWriteResponseMergesAggregatorNotes(t *testing.T) {
	w := httptest.NewRecorder()
	resp := &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}
	writeResponse(w, proto.MustInbound("anthropic"), nil, resp, false,
		[]string{ir.RewrapNote(1)})
	h := w.Header().Get("X-ModelSurge-Notes")
	if !strings.Contains(h, "rewrapped 1 malformed tool call argument(s)") {
		t.Errorf("聚合期注记未进头：%q", h)
	}
}

// 干净响应 + 空聚合注记：头必须不存在（安静是常态，空串头也是噪声）。
func TestWriteResponseCleanHasNoNotesHeader(t *testing.T) {
	w := httptest.NewRecorder()
	resp := &ir.Response{ID: "m1", Model: "m", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}
	writeResponse(w, proto.MustInbound("anthropic"), nil, resp, false, nil)
	if h := w.Header().Get("X-ModelSurge-Notes"); h != "" {
		t.Errorf("干净响应不应有注记头：%q", h)
	}
}

// 流式兜底（上游给整份 JSON、客户端要 SSE）：注记写不进头，
// 必须以 SSE 注释帧出现在流尾。
func TestWriteResponseStreamEmitsNoteFrames(t *testing.T) {
	w := httptest.NewRecorder()
	writeResponse(w, proto.MustInbound("anthropic"), nil, r55LossyResp(), true,
		[]string{ir.RewrapNote(1)})
	body := w.Body.String()
	if !strings.Contains(body, ": modelsurge-note: dropped 1 thought signature(s)") {
		t.Errorf("签名损耗注释帧缺失：\n%s", body)
	}
	if !strings.Contains(body, ": modelsurge-note: rewrapped 1 malformed") {
		t.Errorf("聚合注记注释帧缺失：\n%s", body)
	}
	if w.Header().Get("X-ModelSurge-Notes") != "" {
		t.Error("流式方向不应尝试写注记头（头已发出）")
	}
}

// 思考抑制（hidethoughts）：非流式扫描报「抑制了几块」，内层的签名损耗
// 不再重复报——思考块已被剥掉，扫描看到的是剥后的响应。
func TestHideThoughtsResponseNotes(t *testing.T) {
	base := proto.MustInbound("anthropic")
	c := withHiddenThoughts(base, hideReq())
	notes := c.ResponseNotes(r55LossyResp())
	if len(notes) != 1 || !strings.Contains(notes[0], "suppressed 1 thinking block(s)") {
		t.Fatalf("抑制注记缺失或重复：%v", notes)
	}
	if strings.Contains(notes[0], "signature") {
		t.Errorf("已剥思考块的签名损耗不应再报：%v", notes)
	}
	// 不要求隐藏时装饰器不生效，原 codec 的扫描原样透出。
	if n := withHiddenThoughts(base, &ir.Request{}).ResponseNotes(r55LossyResp()); len(n) != 1 || !strings.Contains(n[0], "signature") {
		t.Errorf("未隐藏时应只报签名损耗：%v", n)
	}
}

// 思考抑制的流式方向：吞掉的 thinking 块进编码器 Notes，随 SSE 注释帧收尾。
func TestHideThoughtsEncoderNotes(t *testing.T) {
	base := proto.MustInbound("anthropic")
	c := withHiddenThoughts(base, hideReq())
	enc := c.NewStreamEncoder()
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		{Type: ir.EvThinkingDelta, Index: 0, Text: "想"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Type: ir.EvTextDelta, Index: 1, Text: "hi"},
		{Type: ir.EvBlockStop, Index: 1},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	} {
		if _, err := enc.Encode(ev); err != nil {
			t.Fatalf("Encode err=%v", err)
		}
	}
	enc.Finish()
	notes := enc.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "suppressed 1 thinking block(s)") {
		t.Errorf("流式抑制注记缺失：%v", notes)
	}
}

// writeRespNotes 空注记不得碰响应头；有注记时与已有头合并。
func TestWriteRespNotesHeaderSemantics(t *testing.T) {
	w := httptest.NewRecorder()
	writeRespNotes(w, "anthropic", nil)
	if h := w.Header().Get("X-ModelSurge-Notes"); h != "" {
		t.Errorf("空注记写出了头：%q", h)
	}
	writeRespNotes(w, "anthropic", []string{"a", "b"})
	if h := w.Header().Get("X-ModelSurge-Notes"); h != "a; b" {
		t.Errorf("多条注记未合并：%q", h)
	}
	writeRespNotes(w, "anthropic", []string{"c"})
	if h := w.Header().Get("X-ModelSurge-Notes"); h != "a; b; c" {
		t.Errorf("与已有头未合并：%q", h)
	}
}

// 主流式泵的注记收尾依赖编码器 Notes 排干语义；这里用聚合器验证
// writeResponse 的 respNotes 形态与实际聚合器输出同型（防格式漂移）。
func TestAggregatorNotesShapeMatchesDelivery(t *testing.T) {
	agg := ir.NewAggregator()
	agg.Feed(ir.Event{Type: ir.EvMessageStart, MessageID: "m1", Model: "m"})
	agg.Feed(ir.Event{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c", Name: "f"}}})
	agg.Feed(ir.Event{Type: ir.EvToolInput, Index: 0, Text: `{"a": 1`})
	agg.Feed(ir.Event{Type: ir.EvBlockStop, Index: 0})
	agg.Feed(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse})
	agg.Feed(ir.Event{Type: ir.EvMessageStop})
	resp, err := agg.Finish()
	if err != nil {
		t.Fatalf("Finish err=%v", err)
	}
	w := httptest.NewRecorder()
	writeResponse(w, proto.MustInbound("openai-chat"), nil, resp, false, agg.Notes())
	h := w.Header().Get("X-ModelSurge-Notes")
	if !strings.Contains(h, "rewrapped 1 malformed tool call argument(s)") {
		t.Errorf("聚合器注记未随响应头送出：%q", h)
	}
	// chat 是字符串槽位，透传规整后的对象原文——不再重复报挪键。
	if strings.Count(h, "rewrapped") != 1 {
		t.Errorf("挪键被重复报告：%q", h)
	}
}

// —— 端到端投递：三条泵各至少一条通路 ——

func forwardMultiChoiceChat(t *testing.T, upstreamStream, clientStream bool) *httptest.ResponseRecorder {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !upstreamStream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl_1","model":"native","choices":[{"index":1,"message":{"role":"assistant","content":"B"},"finish_reason":"length"},{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":"stop"}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, data := range []string{
			`{"id":"chatcmpl_1","model":"native","choices":[{"index":1,"delta":{"content":"B"}},{"index":0,"delta":{"content":"A"}}]}`,
			`{"id":"chatcmpl_1","model":"native","choices":[{"index":1,"delta":{},"finish_reason":"length"},{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		} {
			_, _ = w.Write([]byte("data: " + data + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(up.Close)

	replay := &thinkNotesReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: "chat-1", Protocol: "openai-chat",
		NativeModel: "native", BaseURL: up.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "m", MaxTokens: 64, Stream: clientStream,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	return w
}

func TestJSONFallbackCarriesDecoderChoiceNote(t *testing.T) {
	w := forwardMultiChoiceChat(t, false, false)
	if h := w.Header().Get("X-ModelSurge-Notes"); !strings.Contains(h, "discarded 1 additional response choice") {
		t.Fatalf("JSON 解码损耗未进响应头：%q", h)
	}
	if body := w.Body.String(); !strings.Contains(body, `"text":"A"`) || strings.Contains(body, `"text":"B"`) {
		t.Fatalf("JSON 主候选选择错误：%s", body)
	}
}

func TestMainCollectPathCarriesDecoderChoiceNote(t *testing.T) {
	w := forwardMultiChoiceChat(t, true, false)
	if h := w.Header().Get("X-ModelSurge-Notes"); !strings.Contains(h, "discarded 1 additional response choice") {
		t.Fatalf("SSE 聚合解码损耗未进响应头：%q", h)
	}
	if body := w.Body.String(); !strings.Contains(body, `"text":"A"`) || strings.Contains(body, `"text":"B"`) {
		t.Fatalf("SSE 聚合主候选选择错误：%s", body)
	}
}

func TestMainStreamPathCarriesDecoderChoiceNote(t *testing.T) {
	w := forwardMultiChoiceChat(t, true, true)
	body := w.Body.String()
	if !strings.Contains(body, ": modelsurge-note: discarded 1 additional response choice") {
		t.Fatalf("SSE 解码损耗注释帧缺失：\n%s", body)
	}
	if !strings.Contains(body, `"text":"A"`) || strings.Contains(body, `"text":"B"`) {
		t.Fatalf("SSE 主候选选择错误：\n%s", body)
	}
}

// 主 SSE 泵：anthropic 上游带真签名，chat 客户端无签名槽位，
// 编码器丢弃计数必须变成流尾的 SSE 注释帧。
func TestMainPumpEmitsNoteFrameForDroppedSignature(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, fr := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"native","content":[],"usage":{"input_tokens":11}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-r55-main"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte(fr + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer up.Close()

	replay := &thinkNotesReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: "anthropic-1", Protocol: "anthropic",
		NativeModel: "native", BaseURL: up.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("openai-chat"), &ir.Request{
		Model: "m", MaxTokens: 64, Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	s := w.Body.String()
	if !strings.Contains(s, ": modelsurge-note: dropped 1 thought signature(s)") {
		t.Fatalf("主泵签名损耗注释帧缺失：\n%s", s)
	}
}

// kiro NDJSON 非流式聚合路：聚合器把截断参数挪键，注记经 collectKiroToClient
// 进响应头。
func TestKiroCollectPathCarriesAggregatorNotes(t *testing.T) {
	var body bytes.Buffer
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"a": 1`},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse},
		{Type: ir.EvMessageStop},
	} {
		if err := json.NewEncoder(&body).Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	replay := &kiroExecuteReplay{
		lease: replayv1.TargetLease{RequestID: "req", GroupID: "group", TargetID: "kiro/public", Protocol: "kiro"},
		body:  io.NopCloser(bytes.NewReader(body.Bytes())),
	}
	forwarder := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	forwarder.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "public", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if h := w.Header().Get("X-ModelSurge-Notes"); !strings.Contains(h, "rewrapped 1 malformed tool call argument(s)") {
		t.Fatalf("kiro 聚合路挪键注记未进响应头：%q", h)
	}
}

// kiro NDJSON 流式路：外族签名被客户端编码器门控，注记落 SSE 注释帧。
func TestKiroStreamPathEmitsNoteFrame(t *testing.T) {
	s := forwardKiroEvents(t, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}},
		{Type: ir.EvThinkingDelta, Index: 0, Text: "想"},
		{Type: ir.EvSigDelta, Index: 0, Text: "sig-r55-kiro", SignatureFrom: "gemini"},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn},
		{Type: ir.EvMessageStop},
	})
	if !strings.Contains(s, ": modelsurge-note: dropped 1 thought signature(s)") {
		t.Fatalf("kiro 流式路签名损耗注释帧缺失：\n%s", s)
	}
}

func TestKiroStreamPathPreservesMalformedToolArgsWithNote(t *testing.T) {
	s := forwardKiroEvents(t, []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "c1", Name: "f"}}},
		{Type: ir.EvToolInput, Index: 0, Text: `{"a": 1`},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopMaxTokens},
		{Type: ir.EvMessageStop},
	})
	if !strings.Contains(s, `\"a\": 1`) {
		t.Fatalf("kiro 流式路畸形原文缺失：\n%s", s)
	}
	if !strings.Contains(s, ": modelsurge-note: preserved 1 malformed tool call argument(s) as raw text") {
		t.Fatalf("kiro 流式路畸形参数注释帧缺失：\n%s", s)
	}
}

// 主 SSE 路非流式客户端：聚合器在 BlockStop 把截断参数挪键，注记必须经
// collectUpstreamToClient 穿线进响应头（codec 看到的是规整后的合法对象，
// 少了这条穿线，非流式主路上挪键损耗彻底不可见）。
func TestMainCollectPathCarriesAggregatorNotes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, fr := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"native","content":[],"usage":{"input_tokens":11}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\": 1"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte(fr + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	defer up.Close()

	replay := &thinkNotesReplay{lease: replayv1.TargetLease{
		RequestID: "req", GroupID: "g", TargetID: "anthropic-1", Protocol: "anthropic",
		NativeModel: "native", BaseURL: up.URL, Credential: "sk-up",
	}}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model: "m", MaxTokens: 64,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")

	if h := w.Header().Get("X-ModelSurge-Notes"); !strings.Contains(h, "rewrapped 1 malformed tool call argument(s)") {
		t.Fatalf("主路聚合挪键注记未进响应头：%q", h)
	}
}
