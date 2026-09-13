// paramlog.go 请求/响应参数日志：每次转发打印四行——
//  1. 请求入口：客户端视角请求参数摘要（relay: request）
//  2. 请求出口：转化后（模型映射/账号覆盖/协议夹紧已应用）的上游视角请求参数摘要（relay: upstream request）
//  3. 响应入口：上游响应摘要（块类型/长度/stop/usage/wire 形态）（relay: upstream response）
//  4. 响应出口：写出到客户端的响应摘要（协议/流式/块/字节/帧/编码错误）（relay: response）
//
// 均不包含消息内容、系统提示、工具 schema 等上下文载荷。
// 与访问日志同开关（access_log）。
package relay

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
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

// respShape 响应形状累计（入口/出口累计器共用）：流式路径逐事件观察，
// 非流式路径从聚合 Response 一次装填。只统计形状与用量，不留存内容。
type respShape struct {
	blocks  map[string]int // 块类型 -> 个数
	textLen int
	thinkN  int
	thinkLn int
	stop    string
	usage   ir.Usage
	hasUsg  bool
}

// observe 流式路径：吸收一个 IR 事件。usage 采用与记账一致的
// start/delta 合并语义（message_start 给输入，message_delta 给输出）。
func (s *respShape) observe(ev ir.Event, model *string) {
	switch ev.Type {
	case ir.EvMessageStart:
		if ev.Model != "" && model != nil {
			*model = ev.Model
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
func (s *respShape) fill(resp *ir.Response) {
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

// String 形状部分摘要（块/长度/stop/usage）。
func (s *respShape) String() string {
	var b strings.Builder
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

// respSummarizer 响应入口累计器：上游侧视角（上游名/native 模型/wire 形态）。
type respSummarizer struct {
	enabled bool
	up      string
	model   string
	wire    string // sse / json（上游响应体形态）
	respShape
}

func newRespSummarizer(enabled bool, up, wire string) *respSummarizer {
	return &respSummarizer{enabled: enabled, up: up, wire: wire, respShape: respShape{blocks: map[string]int{}}}
}

func (s *respSummarizer) observe(ev ir.Event) {
	if !s.enabled {
		return
	}
	s.respShape.observe(ev, &s.model)
}

func (s *respSummarizer) fill(resp *ir.Response) {
	if !s.enabled {
		return
	}
	s.model = resp.Model
	s.respShape.fill(resp)
}

// String 输出摘要行。
func (s *respSummarizer) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "up=%s model=%s wire=%s", s.up, s.model, s.wire)
	b.WriteString(s.respShape.String())
	return b.String()
}

// log 输出摘要（未启用时静默）。
func (s *respSummarizer) log() {
	if !s.enabled {
		return
	}
	log.Printf("relay: upstream response %s", s.String())
}

// clientSummarizer 响应出口累计器：写出到客户端的统计
// （客户端协议/流式/字节/帧/编码错误），块形状与入口同口径。
type clientSummarizer struct {
	enabled bool
	proto   string
	stream  bool
	bytes   int64
	frames  int
	errs    int
	respShape
}

func newClientSummarizer(enabled bool, clientProto string, stream bool) *clientSummarizer {
	return &clientSummarizer{enabled: enabled, proto: clientProto, stream: stream, respShape: respShape{blocks: map[string]int{}}}
}

func (s *clientSummarizer) observe(ev ir.Event) {
	if !s.enabled {
		return
	}
	s.respShape.observe(ev, nil)
}

func (s *clientSummarizer) fill(resp *ir.Response) {
	if !s.enabled {
		return
	}
	s.respShape.fill(resp)
}

// wrote 累计写出字节；frame 累计 SSE 帧；encErr 累计客户端编码失败次数。
func (s *clientSummarizer) wrote(n int) {
	if s.enabled {
		s.bytes += int64(n)
	}
}

func (s *clientSummarizer) framesAdd(n int) {
	if s.enabled {
		s.frames += n
	}
}

func (s *clientSummarizer) encErr() {
	if s.enabled {
		s.errs++
	}
}

// String 输出摘要行。
func (s *clientSummarizer) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "proto=%s stream=%t", s.proto, s.stream)
	b.WriteString(s.respShape.String())
	fmt.Fprintf(&b, " bytes=%d frames=%d errs=%d", s.bytes, s.frames, s.errs)
	return b.String()
}

// log 输出摘要（未启用时静默）。
func (s *clientSummarizer) log() {
	if !s.enabled {
		return
	}
	log.Printf("relay: response %s", s.String())
}
