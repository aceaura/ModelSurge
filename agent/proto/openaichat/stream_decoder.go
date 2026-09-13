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

	nextBlock  int
	textIdx    int
	thinkIdx   int
	openBlocks []int // 已分配未关闭的块序号（按分配顺序）

	tools map[int]*pendingTool // OpenAI tool index -> 状态

	usage        ir.Usage
	finishReason string
	gotFinish    bool
	done         bool
}

type pendingTool struct {
	blockIdx    int
	id, name    string
	started     bool
	pendingArgs []string
}

func (codec) NewStreamDecoder() proto.StreamDecoder {
	return &streamDecoder{textIdx: -1, thinkIdx: -1, tools: map[int]*pendingTool{}}
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
	var out []ir.Event
	if chunk.ID != "" {
		d.id = chunk.ID
	}
	if chunk.Model != "" {
		d.model = chunk.Model
	}
	if !d.started {
		d.started = true
		out = append(out, ir.Event{Type: ir.EvMessageStart, MessageID: d.id, Model: d.model})
	}
	if chunk.Usage != nil {
		d.usage.MergeNonZero(decodeUsage(chunk.Usage))
	}
	for _, ch := range chunk.Choices {
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
		return out
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
	for i, v := range d.openBlocks {
		if v == idx {
			d.openBlocks = append(d.openBlocks[:i], d.openBlocks[i+1:]...)
			break
		}
	}
	return ir.Event{Type: ir.EvBlockStop, Index: idx}
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
	u := d.usage
	out = append(out,
		ir.Event{Type: ir.EvMessageDelta, StopReason: MapFinishReason(d.finishReason), Usage: &u},
		ir.Event{Type: ir.EvMessageStop},
	)
	return out
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
	return out
}
