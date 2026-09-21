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

// kironotes_test.go kiro 路径的有损诊断。
//
// 此前 resolvedCandidate 给 kiro 目标建的候选 codec 为 nil，attemptKiro 也
// 不调 Diagnose：kiro 丢掉 thinking 签名、URL 图片、全部采样参数与并行开关，
// 客户端一条说明都收不到。这里从 Forward 入口跑到响应头。

func kiroLease() replayv1.TargetLease {
	return replayv1.TargetLease{RequestID: "req", GroupID: "g", TargetID: "kiro-1", Protocol: "kiro"}
}

func kiroForward(t *testing.T, req *ir.Request) (*httptest.ResponseRecorder, *kiroExecuteReplay) {
	t.Helper()
	replay := &kiroExecuteReplay{lease: kiroLease()}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), req, "client-key")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	return w, replay
}

func TestKiroPathReportsSamplingAndImageLoss(t *testing.T) {
	temp := 0.7
	w, _ := kiroForward(t, &ir.Request{
		Model:         "claude-sonnet-5",
		MaxTokens:     4096,
		Temperature:   &temp,
		StopSequences: []string{"STOP"},
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "hi"},
			{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
		}}},
	})
	notes := w.Header().Get("X-ModelSurge-Notes")
	for _, want := range []string{"temperature", "stop_sequences", "max_tokens", "inline base64 only"} {
		if !strings.Contains(notes, want) {
			t.Errorf("kiro 诊断缺 %s：%q", want, notes)
		}
	}
}

func TestKiroPathReportsThinkingSignatureLoss(t *testing.T) {
	w, _ := kiroForward(t, &ir.Request{
		Model: "claude-sonnet-5",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "hmm", Signature: "sig", SignatureFrom: "anthropic"}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
		},
	})
	if notes := w.Header().Get("X-ModelSurge-Notes"); !strings.Contains(notes, "thinking signature") {
		t.Errorf("kiro 无签名保真能力，未报告：%q", notes)
	}
}

// 推理风格换算的说明在 kiro 路径同样要汇合进响应头，且换算结果要真的进载荷。
func TestKiroPathCarriesDerivedThinkingNote(t *testing.T) {
	w, replay := kiroForward(t, &ir.Request{
		Model:     "claude-sonnet-5",
		MaxTokens: 100000,
		Thinking:  &ir.ThinkingConfig{Enabled: true, Effort: ir.EffortHigh},
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if notes := w.Header().Get("X-ModelSurge-Notes"); !strings.Contains(notes, "derived thinking budget 80000") {
		t.Errorf("kiro 路径缺换算说明：%q", notes)
	}
	var sent ir.Request
	if err := json.Unmarshal(replay.executeReq.Request, &sent); err != nil {
		t.Fatalf("unmarshal kiro request: %v", err)
	}
	if sent.Thinking == nil || sent.Thinking.BudgetTokens != 80000 {
		t.Errorf("换算结果未进 kiro 载荷：%+v", sent.Thinking)
	}
}

// 无损请求不得凭空生出说明头。
func TestKiroPathSilentWhenLossless(t *testing.T) {
	w, _ := kiroForward(t, &ir.Request{
		Model:    "claude-sonnet-5",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	})
	// 断言头「不存在」而非「为空串」：Set(k, "") 会留下一个空值头，
	// 客户端看到的是「有说明但内容为空」，与无损不是同一回事。
	if _, ok := w.Header()["X-Modelsurge-Notes"]; ok {
		t.Errorf("无损请求不应有说明头：%v", w.Header())
	}
}

// 严格 tool_choice 分支走的是 attemptKiroStrict，与主分支两条独立出口，
// 说明头必须在两边都落。
func TestKiroStrictPathWritesNotes(t *testing.T) {
	temp := 0.7
	// 严格分支会校验上游是否真的调了指定工具，不满足就 502 重试后失败；
	// 夹具须返回一次 tool_use，否则测不到写头那一步。
	var nd bytes.Buffer
	for _, ev := range []ir.Event{
		{Type: ir.EvMessageStart, MessageID: "msg", Model: "public"},
		{Type: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "tu_1", Name: "t", Input: json.RawMessage(`{}`)}}},
		{Type: ir.EvBlockStop, Index: 0},
		{Type: ir.EvMessageDelta, StopReason: ir.StopToolUse, Usage: &ir.Usage{OutputTokens: 1}},
		{Type: ir.EvMessageStop},
	} {
		_ = json.NewEncoder(&nd).Encode(ev)
	}
	replay := &kiroExecuteReplay{lease: kiroLease(), body: io.NopCloser(bytes.NewReader(nd.Bytes()))}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model:       "claude-sonnet-5",
		Temperature: &temp,
		Tools:       []ir.Tool{{Name: "t", Description: "d"}},
		ToolChoice:  &ir.ToolChoice{Mode: ir.ChoiceTool, ToolName: "t"},
		Messages:    []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	if notes := w.Header().Get("X-ModelSurge-Notes"); !strings.Contains(notes, "temperature") {
		t.Errorf("严格分支缺说明头：%q（status=%d）", notes, w.Code)
	}
}

// 账号级 request_overrides 此前对 kiro 静默失效（候选的 ov 为 nil）。
func TestKiroPathAppliesRequestOverrides(t *testing.T) {
	temp := 0.11
	max := 2048
	lease := kiroLease()
	lease.RequestOverrides = &replayv1.RequestOverrides{Temperature: &temp, MaxTokens: &max}
	replay := &kiroExecuteReplay{lease: lease}
	f := NewForwarder(&config.Config{}, replay, nil)
	w := httptest.NewRecorder()
	f.Forward(t.Context(), w, proto.MustInbound("anthropic"), &ir.Request{
		Model:    "claude-sonnet-5",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}}},
	}, "client-key")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var sent ir.Request
	if err := json.Unmarshal(replay.executeReq.Request, &sent); err != nil {
		t.Fatalf("unmarshal kiro request: %v", err)
	}
	if sent.Temperature == nil || *sent.Temperature != temp {
		t.Errorf("temperature 覆盖未生效：%+v", sent.Temperature)
	}
	if sent.MaxTokens != max {
		t.Errorf("max_tokens 覆盖未生效：%d", sent.MaxTokens)
	}
}
