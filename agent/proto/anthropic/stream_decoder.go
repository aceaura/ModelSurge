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
	// sawError 已下发过 EvError。error 是终止事件，之后的 message_stop 与
	// Finish() 的断流兜底都不得再产出收尾事件。
	sawError bool
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
		if len(se.ContentBlock) == 0 {
			return nil, nil
		}
		b, ok := decodeRawBlock(se.ContentBlock)
		if !ok {
			return nil, nil
		}
		// 流式 tool_use 的 input 从 {} 开始，参数经 input_json_delta 续传
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			b.ToolUse.Input = nil
		}
		// server_tool_use 同款：流式开块的 input 是 {}，查询串经 input_json_delta
		// 续传。不清掉的话聚合器把开块的 {} 当完整值，增量事件无处落脚，
		// 聚合结果里查询串整段蒸发。
		if b.Type == ir.BlockServerToolUse && b.ServerToolUse != nil {
			b.ServerToolUse.Input = nil
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
			cs := citationsToIR([]json.RawMessage{se.Delta.Citation})
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
			ev.StopDetails = decodeStopDetails(se.Delta.StopDetails)
		}
		if se.Usage != nil {
			u := convUsage(*se.Usage)
			d.usage.MergeNonZero(u)
			ev.Usage = &u
		}
		return []ir.Event{ev}, nil
	case "message_stop":
		if d.sawError {
			return nil, nil
		}
		d.stopped = true
		return []ir.Event{{Type: ir.EvMessageStop}}, nil
	case "ping":
		return []ir.Event{{Type: ir.EvPing}}, nil
	case "error":
		// error 是终止事件。不置这些标记的话 Finish() 会在错误之后再补
		// message_delta{aborted}+message_stop：客户端先看到错误、又看到一个正常
		// 收尾，而 message_stop 先到时序列还会颠倒成 stop->delta。
		d.sawError = true
		d.stopped = true
		d.messageDeltaSent = true
		e := &ir.Error{Type: ir.ErrTypeConnection, Message: "upstream stream error"}
		if se.Error != nil {
			// 类型缺席时保留规范默认值：覆盖成空串会让下游 RenderStreamError 写出
			// "type":""，客户端无从判断该不该重试。
			if se.Error.Type != "" {
				e.Type = se.Error.Type
			}
			if se.Error.Message != "" {
				e.Message = se.Error.Message
			}
		}
		// 可重试性按类型判，不再一律 true：认证失败要换账号（可重试），
		// 非法请求换谁都会被同样拒绝（不可重试）。
		e.Retryable = ir.StreamRetryable(e.Type)
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
	if u.ServerToolUse != nil {
		out.WebSearchRequests = u.ServerToolUse.WebSearchRequests
		out.WebFetchRequests = u.ServerToolUse.WebFetchRequests
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens
	}
	out.InferenceGeo = u.InferenceGeo
	return out
}

// decodeStopDetails 拒绝分类进 IR。category/explanation 官方可显式 null，
// null 与缺省同归空串（官方注明二者语义相同，见 RefusalStopDetails）。
func decodeStopDetails(sd *stopDetails) *ir.StopDetails {
	if sd == nil {
		return nil
	}
	out := &ir.StopDetails{}
	_ = json.Unmarshal(sd.Category, &out.Category)
	_ = json.Unmarshal(sd.Explanation, &out.Explanation)
	return out
}
