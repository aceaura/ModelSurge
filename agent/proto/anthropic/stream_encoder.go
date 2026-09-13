package anthropic

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// streamEncoder IR 事件 -> Anthropic SSE。
// 维护 block 开合不变式：delta 到达未开启的 index 时自动补 content_block_start；
// Finish 强制关闭所有打开的 block 并补齐终止事件（幂等）。
type streamEncoder struct {
	open             map[int]ir.BlockType
	messageDeltaSent bool
	stopped          bool
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{open: map[int]ir.BlockType{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvMessageStart:
		return [][]byte{sseFrame("message_start", marshal(streamEvent{
			Type:    "message_start",
			Message: &eventMessage{ID: ev.MessageID, Model: ev.Model, Usage: encodeUsagePtr(ev.Usage)},
		}))}, nil
	case ir.EvBlockStart:
		e.open[ev.Index] = blockTypeOf(ev.Block)
		return [][]byte{e.blockStartFrame(ev.Index, ev.Block)}, nil
	case ir.EvTextDelta:
		frames := e.ensureOpen(ev.Index, ir.BlockText)
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "text_delta", Text: ev.Text})), nil
	case ir.EvThinkingDelta:
		frames := e.ensureOpen(ev.Index, ir.BlockThinking)
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "thinking_delta", Thinking: ev.Text})), nil
	case ir.EvSigDelta:
		frames := e.ensureOpen(ev.Index, ir.BlockThinking)
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "signature_delta", Signature: ev.Text})), nil
	case ir.EvToolInput:
		frames := e.ensureOpen(ev.Index, ir.BlockToolUse)
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "input_json_delta", PartialJSON: ev.Text})), nil
	case ir.EvBlockStop:
		if !e.closeBlock(ev.Index) {
			return nil, nil // 未打开的 block，忽略
		}
		return [][]byte{sseFrame("content_block_stop", marshal(streamEvent{Type: "content_block_stop", Index: ev.Index}))}, nil
	case ir.EvMessageDelta:
		e.messageDeltaSent = true
		return [][]byte{sseFrame("message_delta", marshal(streamEvent{
			Type:  "message_delta",
			Delta: &delta{StopReason: UnmapStopReason(ev.StopReason)},
			Usage: encodeUsagePtr(ev.Usage),
		}))}, nil
	case ir.EvMessageStop:
		e.stopped = true
		return [][]byte{sseFrame("message_stop", marshal(streamEvent{Type: "message_stop"}))}, nil
	case ir.EvPing:
		return [][]byte{sseFrame("ping", marshal(streamEvent{Type: "ping"}))}, nil
	case ir.EvError:
		return [][]byte{New().RenderStreamError(ev.Err)}, nil
	}
	return nil, fmt.Errorf("anthropic: encode unknown event %q", ev.Type)
}

// Finish 冲刷：关闭所有打开的 block，补 message_delta + message_stop。
func (e *streamEncoder) Finish() [][]byte {
	var out [][]byte
	idxs := make([]int, 0, len(e.open))
	for i := range e.open {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		out = append(out, sseFrame("content_block_stop", marshal(streamEvent{Type: "content_block_stop", Index: i})))
		delete(e.open, i)
	}
	if !e.messageDeltaSent {
		out = append(out, sseFrame("message_delta", marshal(streamEvent{
			Type:  "message_delta",
			Delta: &delta{StopReason: "end_turn"},
		})))
		e.messageDeltaSent = true
	}
	if !e.stopped {
		out = append(out, sseFrame("message_stop", marshal(streamEvent{Type: "message_stop"})))
		e.stopped = true
	}
	return out
}

// ensureOpen delta 到达未开启的 index 时先补 block_start。
func (e *streamEncoder) ensureOpen(index int, t ir.BlockType) [][]byte {
	if _, ok := e.open[index]; ok {
		return nil
	}
	e.open[index] = t
	return [][]byte{e.blockStartFrame(index, &ir.Block{Type: t})}
}

func (e *streamEncoder) closeBlock(index int) bool {
	if _, ok := e.open[index]; !ok {
		return false
	}
	delete(e.open, index)
	return true
}

func (e *streamEncoder) blockStartFrame(index int, b *ir.Block) []byte {
	var cb block
	if b != nil {
		cb = encodeBlock(*b)
		cb.Text = ""
		if cb.Type == "thinking" {
			cb.Thinking = ""
		}
		if cb.Type == "tool_use" || cb.Type == "server_tool_use" {
			cb.Input = json.RawMessage(`{}`) // 参数经 input_json_delta 增量下发
		}
		// web_search_tool_result 的 content 无增量形态，全量随块开始下发
	} else {
		cb = block{Type: "text"}
	}
	return sseFrame("content_block_start", marshal(streamEvent{Type: "content_block_start", Index: index, ContentBlock: &cb}))
}

func (e *streamEncoder) deltaFrame(index int, d delta) []byte {
	return sseFrame("content_block_delta", marshal(streamEvent{Type: "content_block_delta", Index: index, Delta: &d}))
}

func blockTypeOf(b *ir.Block) ir.BlockType {
	if b == nil {
		return ir.BlockText
	}
	return b.Type
}

func encodeUsagePtr(u *ir.Usage) *usage {
	if u == nil {
		return nil
	}
	return &usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheCreationTokens,
	}
}

// ---- 非流式响应 ----

func (codec) DecodeResponse(body []byte) (*ir.Response, error) {
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	return &ir.Response{
		ID:         r.ID,
		Model:      r.Model,
		Content:    decodeBlocks(r.Content),
		StopReason: MapStopReason(r.StopReason),
		Usage:      convUsage(r.Usage),
	}, nil
}

func (codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	out := response{
		ID:         resp.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      resp.Model,
		Content:    encodeBlocks(resp.Content),
		StopReason: UnmapStopReason(resp.StopReason),
		Usage: usage{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationTokens,
		},
	}
	return json.Marshal(out)
}
