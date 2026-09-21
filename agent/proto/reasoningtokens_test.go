package proto_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// reasoningtokens_test.go 思考用量（reasoning tokens）跨协议投影。
//
// 口径：IR 与 OpenAI 两系把思考算作 output 的子集，Gemini 把 thoughts 与
// candidates 并列（totalTokenCount 是三者之和）。两处编解码都要在这道口径差
// 上做换算，否则思考会被算两遍或整体丢失。

// ---- 上游解码方向：思考用量必须进 IR ----

func TestResponsesDecodesReasoningTokens(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"gpt-5","output":[],"usage":{"input_tokens":100,"output_tokens":60,"total_tokens":160,"output_tokens_details":{"reasoning_tokens":40}}}`)
	resp, err := proto.MustOutbound("openai-responses").DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.ReasoningTokens != 40 {
		t.Errorf("ReasoningTokens = %d, want 40", resp.Usage.ReasoningTokens)
	}
	// 思考是 output 的子集，不能从 OutputTokens 里减掉。
	if resp.Usage.OutputTokens != 60 {
		t.Errorf("OutputTokens = %d, want 60（思考不得从输出里扣除）", resp.Usage.OutputTokens)
	}
}

func TestChatDecodesReasoningTokens(t *testing.T) {
	body := []byte(`{"id":"c1","model":"deepseek-v4","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":60,"total_tokens":160,"completion_tokens_details":{"reasoning_tokens":40}}}`)
	resp, err := proto.MustOutbound("openai-chat").DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.ReasoningTokens != 40 {
		t.Errorf("ReasoningTokens = %d, want 40", resp.Usage.ReasoningTokens)
	}
	if resp.Usage.OutputTokens != 60 {
		t.Errorf("OutputTokens = %d, want 60（思考不得从输出里扣除）", resp.Usage.OutputTokens)
	}
}

// 上游没报思考用量时不能凭空造值。
func TestDecodeWithoutDetailsLeavesReasoningZero(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"openai-responses", `{"id":"r","model":"m","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`},
		{"openai-chat", `{"id":"c","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, err := proto.MustOutbound(c.name).DecodeResponse([]byte(c.body))
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			if resp.Usage.ReasoningTokens != 0 {
				t.Errorf("ReasoningTokens = %d, want 0", resp.Usage.ReasoningTokens)
			}
		})
	}
}

// 流式与非流式必须解出同一份用量：两条路径各有自己的 usage 读取点。
func TestResponsesStreamDecodesReasoningTokens(t *testing.T) {
	got := streamUsage(t, "openai-responses", [][2]string{
		{"response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":100,"output_tokens":60,"output_tokens_details":{"reasoning_tokens":40}}}}`},
	})
	if got.ReasoningTokens != 40 {
		t.Errorf("流式 ReasoningTokens = %d, want 40", got.ReasoningTokens)
	}
	if got.OutputTokens != 60 {
		t.Errorf("流式 OutputTokens = %d, want 60", got.OutputTokens)
	}
}

func TestChatStreamDecodesReasoningTokens(t *testing.T) {
	got := streamUsage(t, "openai-chat", [][2]string{
		{"", `{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":60,"completion_tokens_details":{"reasoning_tokens":40}}}`},
	})
	if got.ReasoningTokens != 40 {
		t.Errorf("流式 ReasoningTokens = %d, want 40", got.ReasoningTokens)
	}
	if got.OutputTokens != 60 {
		t.Errorf("流式 OutputTokens = %d, want 60", got.OutputTokens)
	}
}

// streamUsage 喂完整个流后汇总用量。终止事件的产出点因协议而异
// （Responses 在 response.completed 的 Feed 里就发完，Chat 靠 Finish 补），
// 所以两边都要收。
func streamUsage(t *testing.T, name string, frames [][2]string) ir.Usage {
	t.Helper()
	d := proto.MustOutbound(name).NewStreamDecoder()
	var out ir.Usage
	merge := func(evs []ir.Event) {
		for _, ev := range evs {
			if ev.Usage != nil {
				out.MergeNonZero(*ev.Usage)
			}
		}
	}
	for _, f := range frames {
		evs, err := d.Feed(f[0], f[1])
		if err != nil {
			t.Fatalf("Feed(%q): %v", f[0], err)
		}
		merge(evs)
	}
	merge(d.Finish())
	return out
}

// ---- 客户端编码方向：思考用量必须出现在 wire 上 ----

func TestResponsesEncodesReasoningTokens(t *testing.T) {
	body, err := proto.MustInbound("openai-responses").EncodeResponse(reasoningResp())
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var got struct {
		Usage struct {
			OutputTokens        int `json:"output_tokens"`
			OutputTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if got.Usage.OutputTokensDetails == nil {
		t.Fatalf("没有 output_tokens_details：%s", body)
	}
	if got.Usage.OutputTokensDetails.ReasoningTokens != 40 {
		t.Errorf("reasoning_tokens = %d, want 40", got.Usage.OutputTokensDetails.ReasoningTokens)
	}
	if got.Usage.OutputTokens != 60 {
		t.Errorf("output_tokens = %d, want 60（思考是子集，输出不减）", got.Usage.OutputTokens)
	}
}

func TestChatEncodesReasoningTokens(t *testing.T) {
	body, err := proto.MustInbound("openai-chat").EncodeResponse(reasoningResp())
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var got struct {
		Usage struct {
			CompletionTokens        int `json:"completion_tokens"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if got.Usage.CompletionTokensDetails == nil {
		t.Fatalf("没有 completion_tokens_details：%s", body)
	}
	if got.Usage.CompletionTokensDetails.ReasoningTokens != 40 {
		t.Errorf("reasoning_tokens = %d, want 40", got.Usage.CompletionTokensDetails.ReasoningTokens)
	}
	if got.Usage.CompletionTokens != 60 {
		t.Errorf("completion_tokens = %d, want 60（思考是子集，输出不减）", got.Usage.CompletionTokens)
	}
}

// 思考用量为零时不该凭空冒出一个空的 details 壳。reasoning_tokens 本身带
// omitempty，只查这个键的缺席会漏掉 "output_tokens_details":{} —— 空壳会让
// 客户端把「上游没报思考用量」读成「上游报了 0」，所以连容器键一起查。
func TestEncodeOmitsDetailsWithoutReasoning(t *testing.T) {
	resp := reasoningResp()
	resp.Usage.ReasoningTokens = 0
	for name, container := range map[string]string{
		"openai-responses": "output_tokens_details",
		"openai-chat":      "completion_tokens_details",
	} {
		t.Run(name, func(t *testing.T) {
			body, err := proto.MustInbound(name).EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse: %v", err)
			}
			if strings.Contains(string(body), "reasoning_tokens") {
				t.Errorf("无思考用量却出现 reasoning_tokens：%s", body)
			}
			if strings.Contains(string(body), container) {
				t.Errorf("留下了空的 %s 壳：%s", container, body)
			}
		})
	}
}

// ---- Gemini 口径换算 ----

func TestGeminiSplitsThoughtsOutOfCandidates(t *testing.T) {
	m := geminiUsage(t, reasoningResp())
	// candidates 必须扣掉思考部分，否则 total 会把思考算两遍。
	if m.CandidatesTokenCount != 20 {
		t.Errorf("candidatesTokenCount = %d, want 20（60 输出 - 40 思考）", m.CandidatesTokenCount)
	}
	if m.ThoughtsTokenCount != 40 {
		t.Errorf("thoughtsTokenCount = %d, want 40", m.ThoughtsTokenCount)
	}
	if m.PromptTokenCount != 110 {
		t.Errorf("promptTokenCount = %d, want 110（100 输入 + 10 缓存读）", m.PromptTokenCount)
	}
	// Gemini 的 total 是 prompt + candidates + thoughts 三者之和。
	if want := m.PromptTokenCount + m.CandidatesTokenCount + m.ThoughtsTokenCount; m.TotalTokenCount != want {
		t.Errorf("totalTokenCount = %d, want %d（prompt+candidates+thoughts）", m.TotalTokenCount, want)
	}
	if m.TotalTokenCount != 170 {
		t.Errorf("totalTokenCount = %d, want 170", m.TotalTokenCount)
	}
}

// 上游偶发把思考报得比输出还大（口径不一致或分块统计），不能算出负数。
func TestGeminiClampsCandidatesAtZero(t *testing.T) {
	resp := reasoningResp()
	resp.Usage.OutputTokens = 30
	resp.Usage.ReasoningTokens = 50
	m := geminiUsage(t, resp)
	if m.CandidatesTokenCount != 0 {
		t.Errorf("candidatesTokenCount = %d, want 0（不得为负）", m.CandidatesTokenCount)
	}
	if m.ThoughtsTokenCount != 50 {
		t.Errorf("thoughtsTokenCount = %d, want 50", m.ThoughtsTokenCount)
	}
}

func TestGeminiOmitsThoughtsWithoutReasoning(t *testing.T) {
	resp := reasoningResp()
	resp.Usage.ReasoningTokens = 0
	body, err := proto.MustInbound("gemini").EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if strings.Contains(string(body), "thoughtsTokenCount") {
		t.Errorf("无思考用量却出现 thoughtsTokenCount：%s", body)
	}
	m := geminiUsage(t, resp)
	if m.CandidatesTokenCount != 60 {
		t.Errorf("candidatesTokenCount = %d, want 60（无思考时等于全部输出）", m.CandidatesTokenCount)
	}
}

// 流式与非流式两条写出路径共用同一换算。
func TestGeminiStreamCarriesThoughts(t *testing.T) {
	u := reasoningResp().Usage
	out := streamOut(t, "gemini", []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "m1", Model: "gemini-3-pro"},
		{Type: ir.EvTextDelta, Index: 0, Text: "hi"},
		{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &u},
		{Type: ir.EvMessageStop},
	})
	if !strings.Contains(out, `"thoughtsTokenCount":40`) {
		t.Errorf("流里没有 thoughtsTokenCount=40：\n%s", out)
	}
	if !strings.Contains(out, `"candidatesTokenCount":20`) {
		t.Errorf("流里 candidates 未扣掉思考：\n%s", out)
	}
}

// ---- IR 合并语义 ----

func TestMergeNonZeroCarriesReasoning(t *testing.T) {
	u := ir.Usage{InputTokens: 10, OutputTokens: 20, ReasoningTokens: 5}
	u.MergeNonZero(ir.Usage{OutputTokens: 60, ReasoningTokens: 40})
	if u.ReasoningTokens != 40 {
		t.Errorf("ReasoningTokens = %d, want 40", u.ReasoningTokens)
	}
	// 零值不得擦除已有值：流里 usage 分多帧到达，后帧常只带部分字段。
	u.MergeNonZero(ir.Usage{OutputTokens: 70})
	if u.ReasoningTokens != 40 {
		t.Errorf("零值擦除了已有思考用量：%d", u.ReasoningTokens)
	}
}

// 思考是 output 的子集，不能进 TotalInput，也不能让输入口径变动。
func TestReasoningTokensNotCountedInTotalInput(t *testing.T) {
	u := ir.Usage{InputTokens: 100, CacheReadTokens: 10, OutputTokens: 60, ReasoningTokens: 40}
	if got := u.TotalInput(); got != 110 {
		t.Errorf("TotalInput() = %d, want 110", got)
	}
}

func reasoningResp() *ir.Response {
	return &ir.Response{
		ID:         "resp_1",
		Model:      "gpt-5",
		Content:    []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		StopReason: ir.StopEndTurn,
		Usage:      ir.Usage{InputTokens: 100, CacheReadTokens: 10, OutputTokens: 60, ReasoningTokens: 40},
	}
}

type geminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

func geminiUsage(t *testing.T, resp *ir.Response) geminiUsageMetadata {
	t.Helper()
	body, err := proto.MustInbound("gemini").EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	var got struct {
		UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if got.UsageMetadata == nil {
		t.Fatalf("没有 usageMetadata：%s", body)
	}
	return *got.UsageMetadata
}
