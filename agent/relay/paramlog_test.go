package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/aceaura/ModelSurge/agent/ir"
)

func TestRequestParams(t *testing.T) {
	req := &ir.Request{Model: "glm-5.3", Stream: true, MaxTokens: 8192, Messages: make([]ir.Message, 3)}
	s := requestParams("anthropic", req)
	for _, want := range []string{
		"proto=anthropic", "model=glm-5.3", "stream=true", "max_tokens=8192",
		"msgs=3", "thinking=absent",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("requestParams missing %q in %q", want, s)
		}
	}
	if strings.Contains(s, "tools=") || strings.Contains(s, "system=") {
		t.Errorf("unset fields should be omitted: %q", s)
	}

	temp := 0.7
	req.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: 4096}
	req.Temperature = &temp
	req.Tools = []ir.Tool{{Name: "bash"}}
	s = requestParams("openai-chat", req)
	for _, want := range []string{"thinking=on budget=4096", "temp=0.7", "tools=1", "proto=openai-chat"} {
		if !strings.Contains(s, want) {
			t.Errorf("requestParams missing %q in %q", want, s)
		}
	}

	req.Thinking = &ir.ThinkingConfig{Enabled: false}
	if s = requestParams("gemini", req); !strings.Contains(s, "thinking=off") {
		t.Errorf("explicitly disabled thinking should log off: %q", s)
	}
}

// 显式关（effort=none）要在日志里看得出档位：光一个 thinking=off 分不清
// 「客户端显式要求不思考」（线上会写出 none）与「只给了摘要偏好、没表态档位」
// （线上压根不写档位），排查上游为什么思考/不思考时无从下手。
func TestRequestParamsLogsExplicitNoneEffort(t *testing.T) {
	req := &ir.Request{Model: "gpt-5.6-sol", Messages: make([]ir.Message, 1)}
	req.Thinking = &ir.ThinkingConfig{Enabled: false, Effort: ir.EffortNone}
	s := requestParams("openai-chat", req)
	if !strings.Contains(s, "thinking=off effort=none") {
		t.Errorf("显式 none 没进参数日志：%q", s)
	}
	// 没表态档位时不得凭空打出 effort= 键。
	req.Thinking = &ir.ThinkingConfig{Enabled: false}
	if s = requestParams("openai-responses", req); strings.Contains(s, "effort=") {
		t.Errorf("没给档位却打出 effort：%q", s)
	}
}

func TestRespSummarizerEvents(t *testing.T) {
	s := newRespSummarizer(true, "req-1", "ark-6", "sse", time.Now())
	s.observe(ir.Event{Type: ir.EvMessageStart, Model: "glm-5.3-flash", Usage: &ir.Usage{InputTokens: 100, CacheReadTokens: 50}})
	s.observe(ir.Event{Type: ir.EvBlockStart, Block: &ir.Block{Type: ir.BlockThinking}})
	s.observe(ir.Event{Type: ir.EvThinkingDelta, Text: "hmm"})
	s.observe(ir.Event{Type: ir.EvBlockStop, Index: 0})
	s.observe(ir.Event{Type: ir.EvBlockStart, Block: &ir.Block{Type: ir.BlockText}})
	s.observe(ir.Event{Type: ir.EvTextDelta, Text: "你好"})
	s.observe(ir.Event{Type: ir.EvBlockStop, Index: 1})
	s.observe(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 43}})
	s.log() // 不 panic 即可；断言走 String
	got := s.String()
	for _, want := range []string{
		"up=ark-6", "model=glm-5.3-flash", "wire=sse", "blocks=text:1,thinking:1",
		"think_len=3", "text_len=6", "stop=end_turn",
		"in=100 out=43 cache_read=50",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in %q", want, got)
		}
	}
}

func TestRespSummarizerFill(t *testing.T) {
	s := newRespSummarizer(true, "req-1", "up-1", "json", time.Now())
	s.fill(&ir.Response{
		Model: "m", StopReason: ir.StopMaxTokens,
		Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "think"}},
			{Type: ir.BlockText, Text: "hi"},
		},
		Usage: ir.Usage{InputTokens: 1, OutputTokens: 2, Estimated: true},
	})
	got := s.String()
	for _, want := range []string{"up=up-1", "model=m", "wire=json", "blocks=text:1,thinking:1", "stop=max_tokens", "in=1 out=2", "(est)"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in %q", want, got)
		}
	}
}

func TestRespSummarizerDisabled(t *testing.T) {
	s := newRespSummarizer(false, "req-1", "up", "sse", time.Now())
	s.observe(ir.Event{Type: ir.EvTextDelta, Text: "x"})
	s.fill(&ir.Response{Model: "m", Usage: ir.Usage{InputTokens: 1}})
	if len(s.blocks) != 0 || s.hasUsg || s.textLen != 0 {
		t.Errorf("disabled summarizer must not accumulate state: %+v", s)
	}
	s.log() // 未启用时静默
}

// 响应出口累计器：协议/流式/字节/帧/编码错误，块形状与入口同口径。
func TestClientSummarizer(t *testing.T) {
	s := newClientSummarizer(true, "req-1", "anthropic", true, time.Now())
	s.observe(ir.Event{Type: ir.EvMessageStart, Usage: &ir.Usage{InputTokens: 17}})
	s.observe(ir.Event{Type: ir.EvBlockStart, Block: &ir.Block{Type: ir.BlockText}})
	s.observe(ir.Event{Type: ir.EvTextDelta, Text: "你好"})
	s.observe(ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopEndTurn, Usage: &ir.Usage{OutputTokens: 5}})
	s.framesAdd(4)
	s.wrote(120)
	s.wrote(30)
	s.encErr()
	got := s.String()
	for _, want := range []string{
		"proto=anthropic", "stream=true", "blocks=text:1", "text_len=6",
		"stop=end_turn", "in=17 out=5", "bytes=150", "frames=4", "errs=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("client summary missing %q in %q", want, got)
		}
	}
	dis := newClientSummarizer(false, "req-1", "anthropic", false, time.Now())
	dis.observe(ir.Event{Type: ir.EvTextDelta, Text: "x"})
	dis.wrote(9)
	if dis.bytes != 0 || dis.textLen != 0 {
		t.Errorf("disabled client summarizer must not accumulate: %+v", dis)
	}
}
