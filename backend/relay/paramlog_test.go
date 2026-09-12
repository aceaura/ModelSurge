package relay

import (
	"strings"
	"testing"

	"relayd/backend/ir"
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

func TestRespSummarizerEvents(t *testing.T) {
	s := newRespSummarizer(true, "ark-6")
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
		"up=ark-6", "model=glm-5.3-flash", "blocks=text:1,thinking:1",
		"think_len=3", "text_len=6", "stop=end_turn",
		"in=100 out=43 cache_read=50",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in %q", want, got)
		}
	}
}

func TestRespSummarizerFill(t *testing.T) {
	s := newRespSummarizer(true, "up-1")
	s.fill(&ir.Response{
		Model: "m", StopReason: ir.StopMaxTokens,
		Content: []ir.Block{
			{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: "think"}},
			{Type: ir.BlockText, Text: "hi"},
		},
		Usage: ir.Usage{InputTokens: 1, OutputTokens: 2, Estimated: true},
	})
	got := s.String()
	for _, want := range []string{"up=up-1", "model=m", "blocks=text:1,thinking:1", "stop=max_tokens", "in=1 out=2", "(est)"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q in %q", want, got)
		}
	}
}

func TestRespSummarizerDisabled(t *testing.T) {
	s := newRespSummarizer(false, "up")
	s.observe(ir.Event{Type: ir.EvTextDelta, Text: "x"})
	s.fill(&ir.Response{Model: "m", Usage: ir.Usage{InputTokens: 1}})
	if len(s.blocks) != 0 || s.hasUsg || s.textLen != 0 {
		t.Errorf("disabled summarizer must not accumulate state: %+v", s)
	}
	s.log() // 未启用时静默
}
