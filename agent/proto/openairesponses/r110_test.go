package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// ---- R110-B3 prompt_cache_diagnostics 同族往返 ----

// 官方 response.prompt_cache_diagnostics（cache_hit/cache_miss/... 判别式联合）
// 是响应专属回执：同族往返必须逐字带回，显式 null 不当成有回执。
func TestR110PromptCacheDiagnosticsRoundTrip(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[],` +
		`"prompt_cache_diagnostics":{"type":"cache_hit","cached_tokens":128}}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesPromptCacheDiagnostics) == 0 {
		t.Fatal("prompt_cache_diagnostics 没落进 IR")
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"prompt_cache_diagnostics"`) ||
		!strings.Contains(string(out), `"cache_hit"`) {
		t.Errorf("同族往返丢了 prompt_cache_diagnostics：%s", out)
	}
}

// 显式 null 等同没给：不把 4 字节字面量当成有回执写回。
func TestR110PromptCacheDiagnosticsNullNotAReceipt(t *testing.T) {
	resp, err := New().DecodeResponse([]byte(
		`{"id":"r1","model":"m","status":"completed","output":[],"prompt_cache_diagnostics":null}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesPromptCacheDiagnostics) != 0 {
		t.Fatalf("null 被当成了回执：%s", resp.ResponsesPromptCacheDiagnostics)
	}
	out, _ := New().EncodeResponse(resp)
	if strings.Contains(string(out), "prompt_cache_diagnostics") {
		t.Errorf("null 不该写出键：%s", out)
	}
}

// ---- R110-D4 moderation 同族往返 ----

func TestR110ModerationRoundTrip(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[],` +
		`"moderation":{"input_flagged":false,"output_flagged":true}}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesModeration) == 0 {
		t.Fatal("moderation 没落进 IR")
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"moderation"`) ||
		!strings.Contains(string(out), `"output_flagged":true`) {
		t.Errorf("同族往返丢了 moderation：%s", out)
	}
}

func TestR110ModerationNullNotAReceipt(t *testing.T) {
	resp, err := New().DecodeResponse([]byte(
		`{"id":"r1","model":"m","status":"completed","output":[],"moderation":null}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.ResponsesModeration) != 0 {
		t.Fatalf("null 被当成了回执：%s", resp.ResponsesModeration)
	}
	out, _ := New().EncodeResponse(resp)
	if strings.Contains(string(out), "moderation") {
		t.Errorf("null 不该写出键：%s", out)
	}
}

// ---- R110 completed_at 同族往返（响应专属，不拿本地钟伪造）----

func TestR110CompletedAtRoundTrip(t *testing.T) {
	resp, err := New().DecodeResponse([]byte(
		`{"id":"r1","model":"m","status":"completed","output":[],"created_at":1700000000,"completed_at":1700000999}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.CompletedAt != 1700000999 {
		t.Fatalf("completed_at 没落进 IR：%d", resp.CompletedAt)
	}
	out, err := New().EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"completed_at":1700000999`) {
		t.Errorf("同族往返丢了 completed_at：%s", out)
	}
}

// 上游没给完成时间（零值）时绝不伪造一个本地完成时间：键整个不出现。
func TestR110CompletedAtZeroOmitted(t *testing.T) {
	resp, err := New().DecodeResponse([]byte(`{"id":"r1","model":"m","status":"completed","output":[]}`))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, _ := New().EncodeResponse(resp)
	if strings.Contains(string(out), "completed_at") {
		t.Errorf("零值 completed_at 不该写出（伪造完成时间）：%s", out)
	}
}

// ---- R110-A2b/A2c reasoning content 通道保真 ----

// 官方 reasoning item 有两条独立通道：summary（reasoning_summary_text，摘要）
// 与 content（reasoning_text，加密推理原文）。summary 为空、正文在 content 时，
// 解码必须标记 ContentChannel，否则同族回写会把 content 原文塌进 summary 通道。
func TestR110ReasoningContentChannelDecode(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"INNER"}]}` +
		`]}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	var th *ir.Thinking
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil {
			th = b.Thinking
		}
	}
	if th == nil {
		t.Fatalf("content 通道的推理没解出思考块：%+v", resp.Content)
	}
	if th.Text != "INNER" {
		t.Errorf("正文 = %q, want INNER", th.Text)
	}
	if !th.ContentChannel {
		t.Error("走 content 通道却没标记 ContentChannel，同族回写会塌进 summary")
	}
}

// summary 通道（默认）不标记 ContentChannel：维持历史行为。
func TestR110ReasoningSummaryChannelNotMarked(t *testing.T) {
	body := []byte(`{"id":"r1","model":"m","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"SUMM"}]}` +
		`]}`)
	resp, err := New().DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking && b.Thinking != nil {
			if b.Thinking.ContentChannel {
				t.Error("summary 通道被误标成 ContentChannel")
			}
			if b.Thinking.Text != "SUMM" {
				t.Errorf("正文 = %q, want SUMM", b.Thinking.Text)
			}
			return
		}
	}
	t.Fatalf("summary 通道没解出思考块：%+v", resp.Content)
}

// 编码回写：ContentChannel=true 写 content 数组（reasoning_text）+ 空 summary
// 占位（官方 summary required）；false 写 summary_text。
func TestR110ReasoningContentChannelEncode(t *testing.T) {
	enc := func(cc bool) string {
		out, err := New().EncodeResponse(&ir.Response{
			ID: "r1", Model: "m", StopReason: ir.StopEndTurn,
			Content: []ir.Block{{Type: ir.BlockThinking,
				Thinking: &ir.Thinking{Text: "INNER", ContentChannel: cc, ItemID: "rs_1"}}},
		})
		if err != nil {
			t.Fatalf("EncodeResponse: %v", err)
		}
		return string(out)
	}
	content := enc(true)
	if !strings.Contains(content, `"reasoning_text"`) || !strings.Contains(content, `"INNER"`) {
		t.Errorf("ContentChannel=true 没写回 content 通道：%s", content)
	}
	if strings.Contains(content, `"summary_text"`) {
		t.Errorf("ContentChannel=true 不该把原文塞进 summary：%s", content)
	}
	summary := enc(false)
	if !strings.Contains(summary, `"summary_text"`) || strings.Contains(summary, `"reasoning_text"`) {
		t.Errorf("ContentChannel=false 应走 summary 通道：%s", summary)
	}
}

// ContentChannel 经 Clone（JSON 往返）必须存活：请求侧 EncodeRequest 会 Clone。
func TestR110ContentChannelSurvivesClone(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant,
		Content: []ir.Block{{Type: ir.BlockThinking,
			Thinking: &ir.Thinking{Text: "INNER", ContentChannel: true}}}}}}
	cloned := req.Clone()
	th := cloned.Messages[0].Content[0].Thinking
	if th == nil || !th.ContentChannel {
		t.Fatalf("ContentChannel 没熬过 Clone：%+v", th)
	}
}

// ---- R110-D1 steered 流式 ----

func TestR110SteeredStreamDecode(t *testing.T) {
	d := New().NewStreamDecoder()
	if _, err := d.Feed("response.created", `{"type":"response.created","response":{"id":"r1","model":"m"}}`); err != nil {
		t.Fatal(err)
	}
	evs, err := d.Feed("response.incomplete",
		`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"steered"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var got ir.StopReason
	for _, ev := range evs {
		if ev.Type == ir.EvMessageDelta {
			got = ev.StopReason
		}
	}
	if got != ir.StopSteered {
		t.Fatalf("流式 steered = %q, want steered", got)
	}
	if unmapIncompleteReason(ir.StopSteered) != "steered" {
		t.Error("unmapIncompleteReason 未回写 steered")
	}
}

// 防御性编译期断言：responseObj 的新字段都带 omitempty（Clone 走 JSON 往返，
// 缺 omitempty 的 RawMessage 会从空变成 4 字节 null）。
func TestR110ResponseObjOmemptypresent(t *testing.T) {
	raw, _ := json.Marshal(responseObj{ID: "r1", Model: "m"})
	s := string(raw)
	for _, k := range []string{"prompt_cache_diagnostics", "moderation", "completed_at", "metadata"} {
		if strings.Contains(s, k) {
			t.Errorf("空 responseObj 不该写出 %s（缺 omitempty）：%s", k, s)
		}
	}
}
