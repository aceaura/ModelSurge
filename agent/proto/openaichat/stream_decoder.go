package openaichat

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// streamDecoder OpenAI chunk 流 -> IR 事件。
// 处理三件难事（对齐 new-api 的实现策略）：
//  1. tool_calls 的 index 是工具内稠密索引，需重映射为全局块序号；
//  2. 首帧可能只带 arguments 不带 id/name，参数先缓存（PendingArguments），
//     id+name 齐了才发 block_start 并冲刷缓存；
//  3. finish_reason 与 usage 分离到达，统一缓存到 Finish 一次性发出。
type streamDecoder struct {
	id, model string
	started   bool
	// tier 档位回显：chunk 都可能携带，首帧没带上时后续帧补上，
	// 随 Finish 的 message_delta 事件交付。
	tier string

	nextBlock  int
	textIdx    int
	thinkIdx   int
	refusalIdx int
	openBlocks []int // 已分配未关闭的块序号（按分配顺序）

	tools map[int]*pendingTool // OpenAI tool index -> 状态

	usage        ir.Usage
	finishReason string
	gotFinish    bool
	done         bool
	// sawError 已下发过 EvError。错误帧是终止帧，Finish() 不得再补
	// message_delta+message_stop，否则客户端在错误之后又看到一个正常收尾。
	sawError       bool
	primaryChoice  int
	choiceSelected bool
	droppedChoices map[int]struct{}
}

type pendingTool struct {
	blockIdx    int
	id, name    string
	started     bool
	pendingArgs []string
}

func (codec) NewStreamDecoder() proto.StreamDecoder {
	return &streamDecoder{
		textIdx: -1, thinkIdx: -1, refusalIdx: -1,
		tools: map[int]*pendingTool{}, droppedChoices: map[int]struct{}{},
	}
}

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	if data == "[DONE]" {
		d.done = true
		return nil, nil
	}
	var chunk response
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, fmt.Errorf("openai-chat: decode chunk: %w", err)
	}
	// 错误帧必须在开流之前判掉：它没有 choices，落到下面会先补一个 message_start，
	// 再由 Finish() 报 aborted——一次上游拒绝于是变成「客户端收到一个空回答」，
	// 而 relay 因为没有 EvError 会认为请求成功，既不重试也不换账号。
	if chunk.Error != nil {
		d.done = true
		d.sawError = true
		e := &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream stream error"}
		if chunk.Error.Type != "" {
			e.Type = chunk.Error.Type
		}
		if chunk.Error.Message != "" {
			e.Message = chunk.Error.Message
		}
		e.Code = chunk.Error.Code
		e.Retryable = ir.StreamRetryable(e.Type)
		return []ir.Event{{Type: ir.EvError, Err: e}}, nil
	}
	var out []ir.Event
	if chunk.ID != "" {
		d.id = chunk.ID
	}
	if chunk.Model != "" {
		d.model = chunk.Model
	}
	if chunk.ServiceTier != "" {
		d.tier = chunk.ServiceTier
	}
	if !d.started {
		d.started = true
		out = append(out, ir.Event{Type: ir.EvMessageStart, MessageID: d.id, Model: d.model, ServiceTier: d.tier})
	}
	if chunk.Usage != nil {
		d.usage.MergeNonZero(decodeUsage(chunk.Usage))
	}
	if !d.choiceSelected && len(chunk.Choices) > 0 {
		d.primaryChoice = chunk.Choices[0].Index
		foundZero := d.primaryChoice == 0
		for _, ch := range chunk.Choices[1:] {
			if ch.Index == 0 {
				d.primaryChoice = 0
				foundZero = true
				break
			}
			if !foundZero && ch.Index < d.primaryChoice {
				d.primaryChoice = ch.Index
			}
		}
		d.choiceSelected = true
	}
	for _, ch := range chunk.Choices {
		if ch.Index != d.primaryChoice {
			d.droppedChoices[ch.Index] = struct{}{}
			continue
		}
		if ch.Delta != nil {
			out = append(out, d.feedDelta(ch.Delta)...)
		}
		if ch.FinishReason != "" {
			d.gotFinish = true
			d.finishReason = ch.FinishReason
		}
	}
	return out, nil
}

func (d *streamDecoder) feedDelta(m *message) []ir.Event {
	var out []ir.Event
	if m.ReasoningContent != "" {
		if d.thinkIdx == -1 {
			out = append(out, d.openBlock(ir.BlockThinking, &d.thinkIdx)...)
		}
		out = append(out, ir.Event{Type: ir.EvThinkingDelta, Index: d.thinkIdx, Text: m.ReasoningContent})
	}
	if m.Refusal != "" {
		// 拒绝正文自成一块：并入 text 块会让客户端把拒绝渲染成普通回答，
		// 只凭 finish_reason 无法区分。
		if d.refusalIdx == -1 {
			out = append(out, d.openBlock(ir.BlockRefusal, &d.refusalIdx)...)
		}
		out = append(out, ir.Event{Type: ir.EvTextDelta, Index: d.refusalIdx, Text: m.Refusal})
	}
	if len(m.Content) > 0 {
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil && text != "" {
			// thinking -> text 切换时关闭 thinking 块
			if d.thinkIdx != -1 {
				out = append(out, d.closeBlock(d.thinkIdx))
				d.thinkIdx = -1
			}
			if d.textIdx == -1 {
				out = append(out, d.openBlock(ir.BlockText, &d.textIdx)...)
			}
			out = append(out, ir.Event{Type: ir.EvTextDelta, Index: d.textIdx, Text: text})
		}
	}
	// 标注在正文之后到达，落到已开的 text 块上。text 块还没开时丢弃：
	// 为标注开一个空文本块会让客户端多出一段空正文，而偏移量也无从对应。
	if cs := decodeAnnotations(m.Annotations); len(cs) > 0 && d.textIdx != -1 {
		out = append(out, ir.Event{Type: ir.EvCitation, Index: d.textIdx, Citations: cs})
	}
	for _, tc := range m.ToolCalls {
		out = append(out, d.feedToolCall(tc)...)
	}
	return out
}

func (d *streamDecoder) feedToolCall(tc toolCall) []ir.Event {
	var out []ir.Event
	// text 块还开着则先关闭（OpenAI 流里文本先于工具）
	if d.textIdx != -1 {
		out = append(out, d.closeBlock(d.textIdx))
		d.textIdx = -1
	}
	if d.thinkIdx != -1 {
		out = append(out, d.closeBlock(d.thinkIdx))
		d.thinkIdx = -1
	}
	pt := d.tools[tc.Index]
	if pt != nil && tc.ID != "" && pt.id != "" && pt.id != tc.ID {
		// 同一 index 带着新 id 回来：上游把槽位复用给了下一个调用
		// （部分国产兼容端会这样发，new-api 也按此形态处理）。
		// 不拆开会把两次调用的参数粘成一次，且第二次调用的 id 被吞掉。
		if pt.started {
			out = append(out, d.closeBlock(pt.blockIdx))
		} else {
			// 身份还没齐就被顶替：前一个调用永远不会有块，移出待关列表。
			d.dropOpen(pt.blockIdx)
		}
		delete(d.tools, tc.Index)
		pt = nil
	}
	if pt == nil {
		pt = &pendingTool{blockIdx: d.nextBlock}
		d.nextBlock++
		d.openBlocks = append(d.openBlocks, pt.blockIdx)
		d.tools[tc.Index] = pt
	}
	if tc.ID != "" {
		pt.id = tc.ID
	}
	if tc.Function.Name != "" {
		pt.name = tc.Function.Name
	}
	if !pt.started && pt.id != "" && pt.name != "" {
		pt.started = true
		out = append(out, ir.Event{Type: ir.EvBlockStart, Index: pt.blockIdx, Block: &ir.Block{
			Type:    ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: pt.id, Name: pt.name},
		}})
		for _, frag := range pt.pendingArgs {
			out = append(out, ir.Event{Type: ir.EvToolInput, Index: pt.blockIdx, Text: frag})
		}
		pt.pendingArgs = nil
		// 不提前返回：本帧可能同帧携带 arguments（GLM/智谱形态），需续走下方参数处理
	}
	if tc.Function.Arguments != "" {
		if pt.started {
			out = append(out, ir.Event{Type: ir.EvToolInput, Index: pt.blockIdx, Text: tc.Function.Arguments})
		} else {
			pt.pendingArgs = append(pt.pendingArgs, tc.Function.Arguments)
		}
	}
	return out
}

func (d *streamDecoder) openBlock(t ir.BlockType, slot *int) []ir.Event {
	*slot = d.nextBlock
	d.nextBlock++
	d.openBlocks = append(d.openBlocks, *slot)
	return []ir.Event{{Type: ir.EvBlockStart, Index: *slot, Block: &ir.Block{Type: t}}}
}

func (d *streamDecoder) closeBlock(idx int) ir.Event {
	d.dropOpen(idx)
	return ir.Event{Type: ir.EvBlockStop, Index: idx}
}

func (d *streamDecoder) dropOpen(idx int) {
	for i, v := range d.openBlocks {
		if v == idx {
			d.openBlocks = append(d.openBlocks[:i], d.openBlocks[i+1:]...)
			break
		}
	}
}

// Finish 冲刷：关闭所有未闭合块，补 message_delta + message_stop。
func (d *streamDecoder) Finish() []ir.Event {
	if !d.started {
		return nil
	}
	var out []ir.Event
	// 未宣告完成的工具调用也要关闭
	for _, idx := range d.openBlocks {
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: idx})
	}
	d.openBlocks = nil
	if d.sawError {
		// 错误帧已是终止帧：块照关（不给下游留永不结束的块），但不再补收尾事件，
		// 否则客户端在错误之后又看到一个正常结束。
		return out
	}
	u := d.usage
	// 一个 finish_reason 都没收到就断了：异常中断，不能报成 stop 档。
	stop := ir.StopAborted
	if d.gotFinish {
		stop = MapFinishReason(d.finishReason)
	}
	out = append(out,
		ir.Event{Type: ir.EvMessageDelta, StopReason: stop, Usage: &u, ServiceTier: d.tier},
		ir.Event{Type: ir.EvMessageStop},
	)
	return out
}

func (d *streamDecoder) Notes() []string {
	if len(d.droppedChoices) == 0 {
		return nil
	}
	n := len(d.droppedChoices)
	d.droppedChoices = map[int]struct{}{}
	return []string{proto.AdditionalChoicesDropNote(n)}
}

// decodeUsage OpenAI usage -> IR（input 口径换算：prompt 含 cached，需拆出）。
func decodeUsage(u *usage) ir.Usage {
	out := ir.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
		out.InputTokens -= u.PromptTokensDetails.CachedTokens
		if out.InputTokens < 0 {
			out.InputTokens = 0
		}
	}
	if u.CompletionTokensDetails != nil {
		// 思考消耗是 completion 的子集，不从 OutputTokens 里减。
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}
