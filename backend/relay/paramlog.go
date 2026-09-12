// paramlog.go 请求/响应参数日志：每次转发打印两行——
// 请求参数摘要（协议归一后的 IR 视角）与响应摘要（块类型/长度/stop/usage）。
// 均不包含消息内容、系统提示、工具 schema 等上下文载荷。
// 与访问日志同开关（access_log）。
package relay

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"relayd/backend/ir"
)

// requestParams 请求参数摘要（clientProto 为客户端协议名）。
func requestParams(clientProto string, req *ir.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "proto=%s model=%s stream=%t max_tokens=%d msgs=%d",
		clientProto, req.Model, req.Stream, req.MaxTokens, len(req.Messages))
	if req.Temperature != nil {
		fmt.Fprintf(&b, " temp=%g", *req.Temperature)
	}
	if req.TopP != nil {
		fmt.Fprintf(&b, " top_p=%g", *req.TopP)
	}
	if req.TopK != nil {
		fmt.Fprintf(&b, " top_k=%d", *req.TopK)
	}
	if n := len(req.StopSequences); n > 0 {
		fmt.Fprintf(&b, " stop=%d", n)
	}
	if n := len(req.System); n > 0 {
		fmt.Fprintf(&b, " system=%d", n)
	}
	if n := len(req.Tools); n > 0 {
		fmt.Fprintf(&b, " tools=%d", n)
	}
	if tc := req.ToolChoice; tc != nil {
		fmt.Fprintf(&b, " tool_choice=%s", tc.Mode)
	}
	switch {
	case req.Thinking == nil:
		b.WriteString(" thinking=absent")
	case !req.Thinking.Enabled:
		b.WriteString(" thinking=off")
	default:
		fmt.Fprintf(&b, " thinking=on budget=%d", req.Thinking.BudgetTokens)
		if req.Thinking.Effort != "" {
			fmt.Fprintf(&b, " effort=%s", req.Thinking.Effort)
		}
	}
	return b.String()
}

// respSummarizer 响应摘要累计器：流式路径逐事件观察，
// 非流式路径从聚合 Response 一次装填。只统计形状与用量，不留存内容。
type respSummarizer struct {
	enabled bool
	up      string
	model   string
	blocks  map[string]int // 块类型 -> 个数
	textLen int
	thinkN  int
	thinkLn int
	stop    string
	usage   ir.Usage
	hasUsg  bool
}

func newRespSummarizer(enabled bool, up string) *respSummarizer {
	return &respSummarizer{enabled: enabled, up: up, blocks: map[string]int{}}
}

// observe 流式路径：吸收一个 IR 事件。usage 采用与记账一致的
// start/delta 合并语义（message_start 给输入，message_delta 给输出）。
func (s *respSummarizer) observe(ev ir.Event) {
	if !s.enabled {
		return
	}
	switch ev.Type {
	case ir.EvMessageStart:
		if ev.Model != "" {
			s.model = ev.Model
		}
		if ev.Usage != nil {
			s.usage = *ev.Usage
			s.hasUsg = true
		}
	case ir.EvBlockStart:
		if ev.Block != nil {
			s.blocks[string(ev.Block.Type)]++
			if ev.Block.Type == ir.BlockThinking {
				s.thinkN++
			}
		}
	case ir.EvTextDelta:
		s.textLen += len(ev.Text)
	case ir.EvThinkingDelta:
		s.thinkLn += len(ev.Text)
	case ir.EvMessageDelta:
		if ev.StopReason != "" {
			s.stop = string(ev.StopReason)
		}
		if ev.Usage != nil {
			s.usage.MergeNonZero(*ev.Usage)
			s.hasUsg = true
		}
	}
}

// fill 非流式路径：从聚合响应一次装填。
func (s *respSummarizer) fill(resp *ir.Response) {
	if !s.enabled {
		return
	}
	s.model = resp.Model
	s.stop = string(resp.StopReason)
	s.usage = resp.Usage
	s.hasUsg = true
	for _, b := range resp.Content {
		s.blocks[string(b.Type)]++
		switch b.Type {
		case ir.BlockText:
			s.textLen += len(b.Text)
		case ir.BlockThinking:
			s.thinkN++
			if b.Thinking != nil {
				s.thinkLn += len(b.Thinking.Text)
			}
		}
	}
}

// String 输出摘要行。
func (s *respSummarizer) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "up=%s model=%s", s.up, s.model)
	if len(s.blocks) > 0 {
		types := make([]string, 0, len(s.blocks))
		for t := range s.blocks {
			types = append(types, t)
		}
		sort.Strings(types)
		b.WriteString(" blocks=")
		for i, t := range types {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%s:%d", t, s.blocks[t])
		}
	}
	if s.thinkLn > 0 {
		fmt.Fprintf(&b, " think_len=%d", s.thinkLn)
	}
	if s.textLen > 0 {
		fmt.Fprintf(&b, " text_len=%d", s.textLen)
	}
	if s.stop != "" {
		fmt.Fprintf(&b, " stop=%s", s.stop)
	}
	if s.hasUsg {
		fmt.Fprintf(&b, " in=%d out=%d cache_read=%d cache_creation=%d",
			s.usage.InputTokens, s.usage.OutputTokens, s.usage.CacheReadTokens, s.usage.CacheCreationTokens)
		if s.usage.Estimated {
			b.WriteString(" (est)")
		}
	}
	return b.String()
}

// log 输出摘要（未启用时静默）。
func (s *respSummarizer) log() {
	if !s.enabled {
		return
	}
	log.Printf("relay: response %s", s.String())
}
