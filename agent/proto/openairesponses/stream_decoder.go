package openairesponses

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// streamDecoder Responses SSE -> IR 事件。
// output_index 直接作为 IR 块序号；事件映射表对齐 sub2api responses_to_anthropic.go。
type streamDecoder struct {
	started     bool
	sawToolCall bool
	usage       ir.Usage
	stopReason  ir.StopReason
	finished    bool
}

func (codec) NewStreamDecoder() proto.StreamDecoder { return &streamDecoder{} }

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	if data == "[DONE]" {
		d.finished = true
		return nil, nil
	}
	var se streamEvent
	if err := json.Unmarshal([]byte(data), &se); err != nil {
		return nil, fmt.Errorf("openai-responses: decode stream event: %w", err)
	}
	switch se.Type {
	case "response.created":
		d.started = true
		ev := ir.Event{Type: ir.EvMessageStart}
		if se.Response != nil {
			ev.MessageID = se.Response.ID
			ev.Model = se.Response.Model
		}
		return []ir.Event{ev}, nil
	case "response.output_item.added":
		if se.Item == nil {
			return nil, nil
		}
		switch se.Item.Type {
		case "message":
			return []ir.Event{{Type: ir.EvBlockStart, Index: se.OutputIndex, Block: &ir.Block{Type: ir.BlockText}}}, nil
		case "function_call":
			d.sawToolCall = true
			return []ir.Event{{Type: ir.EvBlockStart, Index: se.OutputIndex, Block: &ir.Block{
				Type:    ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: se.Item.CallID, Name: se.Item.Name},
			}}}, nil
		case "reasoning":
			return []ir.Event{{Type: ir.EvBlockStart, Index: se.OutputIndex, Block: &ir.Block{
				Type:     ir.BlockThinking,
				Thinking: &ir.Thinking{},
			}}}, nil
		}
		return nil, nil
	case "response.content_part.added":
		return nil, nil // block 已由 output_item.added 开启
	case "response.output_text.delta", "response.output_text.annotation.added":
		if se.Delta == "" {
			return nil, nil
		}
		return []ir.Event{{Type: ir.EvTextDelta, Index: se.OutputIndex, Text: se.Delta}}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return []ir.Event{{Type: ir.EvThinkingDelta, Index: se.OutputIndex, Text: se.Delta}}, nil
	case "response.function_call_arguments.delta":
		return []ir.Event{{Type: ir.EvToolInput, Index: se.OutputIndex, Text: se.Delta}}, nil
	case "response.output_item.done":
		var out []ir.Event
		// 关 thinking 块前先发 signature_delta（对齐 sub2api :688）
		if se.Item != nil && se.Item.Type == "reasoning" && se.Item.EncryptedContent != "" {
			out = append(out, ir.Event{Type: ir.EvSigDelta, Index: se.OutputIndex, Text: se.Item.EncryptedContent})
		}
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: se.OutputIndex})
		return out, nil
	case "response.completed":
		d.finished = true
		if se.Response != nil && se.Response.Usage != nil {
			d.usage = decodeUsage(se.Response.Usage)
		}
		d.stopReason = ir.StopEndTurn
		if d.sawToolCall {
			d.stopReason = ir.StopToolUse
		}
		return d.terminalEvents(), nil
	case "response.incomplete":
		d.finished = true
		d.stopReason = ir.StopMaxTokens
		if se.Response != nil && se.Response.Usage != nil {
			d.usage = decodeUsage(se.Response.Usage)
		}
		return d.terminalEvents(), nil
	case "response.failed":
		d.finished = true
		e := &ir.Error{Type: ir.ErrTypeUpstream, Message: "upstream response failed", Retryable: true}
		if se.Response != nil && se.Response.Error != nil {
			e.Type = se.Response.Error.Type
			e.Code = se.Response.Error.Code
			e.Message = se.Response.Error.Message
			// 风控拦截不可重试（对齐 sub2api cyber_policy 特例）
			if e.Code == "cyber_policy" || e.Type == "content_filter" {
				e.Retryable = false
				e.Type = ir.ErrTypeContentFilter
			}
		}
		return []ir.Event{{Type: ir.EvError, Err: e}}, nil
	case "error":
		e := &ir.Error{Type: ir.ErrTypeUpstream, Retryable: true}
		if se.Response != nil && se.Response.Error != nil {
			e.Type = se.Response.Error.Type
			e.Code = se.Response.Error.Code
			e.Message = se.Response.Error.Message
		}
		return []ir.Event{{Type: ir.EvError, Err: e}}, nil
	}
	return nil, nil // response.queued / in_progress 等进度事件忽略
}

func (d *streamDecoder) terminalEvents() []ir.Event {
	u := d.usage
	return []ir.Event{
		{Type: ir.EvMessageDelta, StopReason: d.stopReason, Usage: &u},
		{Type: ir.EvMessageStop},
	}
}

// Finish 断流兜底：补齐终止事件。
func (d *streamDecoder) Finish() []ir.Event {
	if !d.started || d.finished {
		return nil
	}
	d.finished = true
	if d.stopReason == "" {
		d.stopReason = ir.StopEndTurn
	}
	return d.terminalEvents()
}

// decodeUsage Responses usage -> IR（input 含 cached，需拆出）。
func decodeUsage(u *usage) ir.Usage {
	out := ir.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
		out.InputTokens -= u.InputTokensDetails.CachedTokens
		if out.InputTokens < 0 {
			out.InputTokens = 0
		}
	}
	return out
}
