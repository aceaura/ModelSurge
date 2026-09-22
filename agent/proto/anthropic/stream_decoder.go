package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// MapStopReason Anthropic stop_reason -> 规范 StopReason。
func MapStopReason(s string) ir.StopReason {
	switch s {
	case "end_turn":
		return ir.StopEndTurn
	case "stop_sequence":
		return ir.StopStopSequence
	case "pause_turn":
		return ir.StopPauseTurn
	case "max_tokens":
		return ir.StopMaxTokens
	case "tool_use":
		return ir.StopToolUse
	case "refusal":
		return ir.StopRefusal
	default:
		return ir.StopEndTurn
	}
}

// UnmapStopReason 规范 StopReason -> Anthropic stop_reason。
func UnmapStopReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens:
		return "max_tokens"
	case ir.StopToolUse:
		return "tool_use"
	case ir.StopRefusal:
		return "refusal"
	case ir.StopStopSequence:
		return "stop_sequence"
	case ir.StopPauseTurn:
		return "pause_turn"
	case ir.StopAborted:
		// Anthropic 没有「中断」档。取 max_tokens 而非 end_turn：两者都表示
		// 输出不完整，客户端至少不会把半截结果当成最终答案（end_turn 会）。
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// streamDecoder Anthropic SSE -> IR 事件。
type streamDecoder struct {
	usage            ir.Usage
	messageDeltaSent bool
	stopped          bool
	started          bool
}

func (codec) NewStreamDecoder() proto.StreamDecoder { return &streamDecoder{} }

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	if data == "[DONE]" {
		return nil, nil
	}
	var se streamEvent
	if err := json.Unmarshal([]byte(data), &se); err != nil {
		return nil, fmt.Errorf("anthropic: decode stream event: %w", err)
	}
	switch se.Type {
	case "message_start":
		d.started = true
		ev := ir.Event{Type: ir.EvMessageStart}
		if se.Message != nil {
			ev.MessageID = se.Message.ID
			ev.Model = se.Message.Model
			ev.ServiceTier = se.Message.ServiceTier
			ev.Container = decodeContainer(se.Message.Container)
			if se.Message.Usage != nil {
				u := convUsage(*se.Message.Usage)
				d.usage.MergeNonZero(u)
				ev.Usage = &u
			}
		}
		return []ir.Event{ev}, nil
	case "content_block_start":
		if se.ContentBlock == nil {
			return nil, nil
		}
		b := decodeBlock(*se.ContentBlock)
		// 流式 tool_use 的 input 从 {} 开始，参数经 input_json_delta 续传
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			b.ToolUse.Input = nil
		}
		return []ir.Event{{Type: ir.EvBlockStart, Index: se.Index, Block: &b}}, nil
	case "content_block_delta":
		if se.Delta == nil {
			return nil, nil
		}
		switch se.Delta.Type {
		case "text_delta":
			return []ir.Event{{Type: ir.EvTextDelta, Index: se.Index, Text: se.Delta.Text}}, nil
		case "input_json_delta":
			return []ir.Event{{Type: ir.EvToolInput, Index: se.Index, Text: se.Delta.PartialJSON}}, nil
		case "thinking_delta":
			return []ir.Event{{Type: ir.EvThinkingDelta, Index: se.Index, Text: se.Delta.Thinking}}, nil
		case "signature_delta":
			return []ir.Event{{Type: ir.EvSigDelta, Index: se.Index, Text: se.Delta.Signature,
				SignatureFrom: ir.SigFrom(Name, se.Delta.Signature)}}, nil
		case "citations_delta":
			cs := decodeCitations([]citation{*orEmptyCitation(se.Delta.Citation)})
			if len(cs) == 0 {
				return nil, nil
			}
			return []ir.Event{{Type: ir.EvCitation, Index: se.Index, Citations: cs}}, nil
		}
		return nil, nil
	case "content_block_stop":
		return []ir.Event{{Type: ir.EvBlockStop, Index: se.Index}}, nil
	case "message_delta":
		d.messageDeltaSent = true
		ev := ir.Event{Type: ir.EvMessageDelta}
		if se.Delta != nil {
			ev.StopReason = MapStopReason(se.Delta.StopReason)
			ev.StopSequence = se.Delta.StopSequence
			// 容器回显也可能落在 message_delta 上（官方 Delta.container）。
			ev.Container = decodeContainer(se.Delta.Container)
		}
		if se.Usage != nil {
			u := convUsage(*se.Usage)
			d.usage.MergeNonZero(u)
			ev.Usage = &u
		}
		return []ir.Event{ev}, nil
	case "message_stop":
		d.stopped = true
		return []ir.Event{{Type: ir.EvMessageStop}}, nil
	case "ping":
		return []ir.Event{{Type: ir.EvPing}}, nil
	case "error":
		e := &ir.Error{Type: ir.ErrTypeUpstream, Retryable: true}
		if se.Error != nil {
			e.Type = se.Error.Type
			e.Message = se.Error.Message
		}
		return []ir.Event{{Type: ir.EvError, Err: e}}, nil
	}
	return nil, nil
}

// Finish 异常断流兜底：补齐 message_delta + message_stop，
// 保证事件序列对下游编码器是完整可收尾的。
func (d *streamDecoder) Finish() []ir.Event {
	var out []ir.Event
	if d.started && !d.messageDeltaSent {
		u := d.usage
		// 上游一个 message_delta 都没给就断了：这是异常中断，不是说完了。
		// 报 end_turn 会让客户端把半截输出当成最终答案而不重试。
		out = append(out, ir.Event{Type: ir.EvMessageDelta, StopReason: ir.StopAborted, Usage: &u})
	}
	if d.started && !d.stopped {
		out = append(out, ir.Event{Type: ir.EvMessageStop})
	}
	return out
}

func convUsage(u usage) ir.Usage {
	out := ir.Usage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
	}
	if u.CacheCreation != nil {
		out.CacheCreation5mTokens = u.CacheCreation.Ephemeral5mInputTokens
		out.CacheCreation1hTokens = u.CacheCreation.Ephemeral1hInputTokens
		out.CacheCreationDetailsKnown = true
		if out.CacheCreationTokens == 0 {
			out.CacheCreationTokens = out.CacheCreation5mTokens + out.CacheCreation1hTokens
		}
	}
	return out
}
