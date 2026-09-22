package openairesponses

import (
	"encoding/json"
	"fmt"
	"strings"

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
	// tier 档位回显：response.created 与 completed/incomplete 都可能携带，
	// 后者晚到时随终止的 message_delta 事件交付。
	tier string
	// toolArgs 记录已发出的参数前缀。done 事件携带完整值时只补缺失后缀，
	// 既覆盖 done-only 上游，又不把已有 delta 重复一遍。
	toolArgs map[int]string
	// refusalOpen 已开启的拒绝块（按 output_index）。上游对被拒绝的消息仍然
	// 先发 output_item.added type=message，从那一帧看不出是拒绝，块只能等
	// response.refusal.delta 到了再补开。
	refusalOpen map[int]bool
}

// refusalBlockBase 拒绝块的 IR 序号偏移。output_index 本身已被同一条 message
// 的文本块占用，拒绝必须落在独立块上（否则会被并进文本块，客户端无法区分），
// 故整体挪到一个不会与 output_index 相撞的区段。
const refusalBlockBase = 1 << 20

func (d *streamDecoder) refusalIndex(outputIndex int) int { return refusalBlockBase + outputIndex }

func (codec) NewStreamDecoder() proto.StreamDecoder {
	return &streamDecoder{refusalOpen: map[int]bool{}, toolArgs: map[int]string{}}
}

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
			ev.ServiceTier = se.Response.ServiceTier
			d.tier = se.Response.ServiceTier
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
			out := []ir.Event{{Type: ir.EvBlockStart, Index: se.OutputIndex, Block: &ir.Block{
				Type:    ir.BlockToolUse,
				ToolUse: &ir.ToolUse{ID: se.Item.CallID, Name: se.Item.Name},
			}}}
			return append(out, d.completeToolArgs(se.OutputIndex, se.Item.Arguments)...), nil
		case "reasoning":
			return []ir.Event{{Type: ir.EvBlockStart, Index: se.OutputIndex, Block: &ir.Block{
				Type:     ir.BlockThinking,
				Thinking: &ir.Thinking{},
			}}}, nil
		}
		return nil, nil
	case "response.content_part.added":
		return nil, nil // block 已由 output_item.added 开启
	case "response.output_text.delta":
		if se.Delta == "" {
			return nil, nil
		}
		return []ir.Event{{Type: ir.EvTextDelta, Index: se.OutputIndex, Text: se.Delta}}, nil
	case "response.output_text.annotation.added":
		// 该事件没有 delta 字段：正文在 annotation 之外。此前与文本增量并档，
		// 于是恒命中 delta 为空的分支被静默丢弃，引用一条都到不了客户端。
		cs := decodeAnnotations([]annotation{*orEmptyAnnotation(se.Annotation)})
		if len(cs) == 0 {
			return nil, nil
		}
		return []ir.Event{{Type: ir.EvCitation, Index: se.OutputIndex, Citations: cs}}, nil
	case "response.refusal.delta":
		// 拒绝正文另开一块：output_item.added 只给出 message 类型，看不出这条
		// 是拒绝，所以块在这里补开。并入既有 text 块会让客户端把拒绝渲染成
		// 普通回答（cc-switch streaming_responses.rs 把它映射成可见内容）。
		if se.Delta == "" {
			return nil, nil
		}
		var out []ir.Event
		if !d.refusalOpen[se.OutputIndex] {
			d.refusalOpen[se.OutputIndex] = true
			out = append(out, ir.Event{Type: ir.EvBlockStart, Index: d.refusalIndex(se.OutputIndex),
				Block: &ir.Block{Type: ir.BlockRefusal}})
		}
		out = append(out, ir.Event{Type: ir.EvTextDelta, Index: d.refusalIndex(se.OutputIndex), Text: se.Delta})
		return out, nil
	case "response.refusal.done":
		if !d.refusalOpen[se.OutputIndex] {
			return nil, nil
		}
		delete(d.refusalOpen, se.OutputIndex)
		return []ir.Event{{Type: ir.EvBlockStop, Index: d.refusalIndex(se.OutputIndex)}}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return []ir.Event{{Type: ir.EvThinkingDelta, Index: se.OutputIndex, Text: se.Delta}}, nil
	case "response.function_call_arguments.delta":
		if se.Delta == "" {
			return nil, nil
		}
		d.toolArgs[se.OutputIndex] += se.Delta
		return []ir.Event{{Type: ir.EvToolInput, Index: se.OutputIndex, Text: se.Delta}}, nil
	case "response.function_call_arguments.done":
		return d.completeToolArgs(se.OutputIndex, se.Arguments), nil
	case "response.output_item.done":
		var out []ir.Event
		if se.Item != nil && se.Item.Type == "function_call" {
			out = append(out, d.completeToolArgs(se.OutputIndex, se.Item.Arguments)...)
			delete(d.toolArgs, se.OutputIndex)
		}
		// 关 thinking 块前先发 signature_delta（对齐 sub2api :688）
		if se.Item != nil && se.Item.Type == "reasoning" && se.Item.EncryptedContent != "" {
			out = append(out, ir.Event{Type: ir.EvSigDelta, Index: se.OutputIndex, Text: se.Item.EncryptedContent,
				SignatureFrom: ir.SigFrom(Name, se.Item.EncryptedContent)})
		}
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: se.OutputIndex})
		return out, nil
	case "response.completed":
		d.finished = true
		if se.Response != nil && se.Response.Usage != nil {
			d.usage = decodeUsage(se.Response.Usage)
		}
		if se.Response != nil && se.Response.ServiceTier != "" {
			d.tier = se.Response.ServiceTier
		}
		d.stopReason = ir.StopEndTurn
		if d.sawToolCall {
			d.stopReason = ir.StopToolUse
		}
		return d.terminalEvents(), nil
	case "response.incomplete":
		d.finished = true
		d.stopReason = mapIncompleteReason(se.Response)
		if se.Response != nil && se.Response.Usage != nil {
			d.usage = decodeUsage(se.Response.Usage)
		}
		if se.Response != nil && se.Response.ServiceTier != "" {
			d.tier = se.Response.ServiceTier
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

// completeToolArgs 用终态完整值补齐尚未收到的参数后缀。done 不是新一份参数，
// 已由 delta 交付的前缀不得重复；终态比当前值短或分叉时也不能追加成畸形 JSON。
func (d *streamDecoder) completeToolArgs(index int, full string) []ir.Event {
	if full == "" {
		return nil
	}
	current := d.toolArgs[index]
	if current == full {
		return nil
	}
	if !strings.HasPrefix(full, current) {
		return nil
	}
	d.toolArgs[index] = full
	return []ir.Event{{Type: ir.EvToolInput, Index: index, Text: strings.TrimPrefix(full, current)}}
}

// mapIncompleteReason 读 incomplete_details.reason 判断截断原因。
// 此前恒判 max_tokens，把风控拦截误报成「输出太长」——客户端据此会加大
// max_output_tokens 重试，而真正要做的是改提示词。
// reason 缺失时仍按 max_tokens（对齐 cc-switch transform_responses.rs:2091）。
func mapIncompleteReason(r *responseObj) ir.StopReason {
	if r != nil && r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "content_filter" {
		return ir.StopRefusal
	}
	return ir.StopMaxTokens
}

// unmapIncompleteReason 规范 StopReason -> incomplete_details.reason；
// 返回空串表示这一档不是截断，status 应为 completed。
// pause_turn 也归到 max_output_tokens：Responses 没有续跑语义，但至少让客户端
// 知道输出不完整（对齐 chat 侧把它映射成 length 的判据）。
func unmapIncompleteReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens, ir.StopPauseTurn, ir.StopAborted:
		return "max_output_tokens"
	case ir.StopRefusal:
		return "content_filter"
	default:
		return ""
	}
}

func (d *streamDecoder) terminalEvents() []ir.Event {
	var out []ir.Event
	// 未收到 refusal.done 就直接终止时补关块：output_item.done 关的是
	// output_index 那个文本块，关不到挪过区段的拒绝块，不补会让下游编码器
	// 认为块还开着，拒绝正文卡在缓冲里发不出去。
	for idx := range d.refusalOpen {
		out = append(out, ir.Event{Type: ir.EvBlockStop, Index: d.refusalIndex(idx)})
	}
	d.refusalOpen = map[int]bool{}
	u := d.usage
	return append(out,
		ir.Event{Type: ir.EvMessageDelta, StopReason: d.stopReason, Usage: &u, ServiceTier: d.tier},
		ir.Event{Type: ir.EvMessageStop},
	)
}

// Finish 断流兜底：补齐终止事件。
func (d *streamDecoder) Finish() []ir.Event {
	if !d.started || d.finished {
		return nil
	}
	d.finished = true
	// 走到这里必然没收到 response.completed / incomplete / failed——那三个分支
	// 都会置 finished，上面已提前返回。所以停止原因只能是中断档。
	d.stopReason = ir.StopAborted
	return d.terminalEvents()
}

// decodeUsage Responses usage -> IR（input 含 cached，需拆出）。
func decodeUsage(u *usage) ir.Usage {
	out := ir.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.OutputTokensDetails != nil {
		// 思考消耗是 output 的子集，不从 OutputTokens 里减。
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
		out.InputTokens -= u.InputTokensDetails.CachedTokens
		if out.InputTokens < 0 {
			out.InputTokens = 0
		}
	}
	return out
}
