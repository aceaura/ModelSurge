package openairesponses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// streamDecoder Responses SSE -> IR 事件。
// 寻址单位是 content part 而不是 output item：Responses 的一条 message 可以带
// 多个 part（output_text / refusal 混排），而 IR 的块是扁平的。事件映射表对齐
// sub2api responses_to_anthropic.go。
type streamDecoder struct {
	started     bool
	sawToolCall bool
	usage       ir.Usage
	stopReason  ir.StopReason
	finished    bool
	// tier 档位回显：response.created 与 completed/incomplete 都可能携带，
	// 后者晚到时随终止的 message_delta 事件交付。
	tier string
	// toolArgs 记录已发出的参数前缀（按 IR 块序号）。done 事件携带完整值时只补
	// 缺失后缀，既覆盖 done-only 上游，又不把已有 delta 重复一遍。
	toolArgs map[int]string
	// parts content part -> IR 块序号。IR 序号由本解码器稠密分配，不等于
	// output_index：一条 message 的多个 part 要各占一块，而 output_index 相同。
	parts map[partKey]int
	// itemParts output_index -> 该 item 下已开的 IR 块序号，按开块顺序。
	// 关块一律走这个顺序，保证下游看到的 start/stop 不交叉嵌套。
	itemParts map[int][]int
	// open 仍开着的 IR 块。关块幂等：content_part.done / refusal.done /
	// output_item.done 会重复指向同一个块。
	open  map[int]bool
	order []int
	next  int
}

// partKey content part 的寻址键。refusal 单独占一位：上游漏发 content_part.added
// 时文本与拒绝会共用同一个 (output_index, content_index)，并成一块等于把拒绝
// 正文渲染成普通回答。
type partKey struct {
	out     int
	content int
	refusal bool
}

func (codec) NewStreamDecoder() proto.StreamDecoder {
	return &streamDecoder{
		toolArgs:  map[int]string{},
		parts:     map[partKey]int{},
		itemParts: map[int][]int{},
		open:      map[int]bool{},
	}
}

// assign 定位 part 对应的 IR 块，尚未开块时按 blk 补开并返回 BlockStart。
// 补开是必需的兼容路径：漏发 content_part.added 的上游照样能出正文，
// 而悬空的增量会让下游编码器直接报错断流。
func (d *streamDecoder) assign(k partKey, blk *ir.Block) (int, []ir.Event) {
	if idx, ok := d.parts[k]; ok {
		return idx, nil
	}
	idx := d.next
	d.next++
	d.parts[k] = idx
	d.itemParts[k.out] = append(d.itemParts[k.out], idx)
	d.open[idx] = true
	d.order = append(d.order, idx)
	return idx, []ir.Event{{Type: ir.EvBlockStart, Index: idx, Block: blk}}
}

func (d *streamDecoder) close(idx int) []ir.Event {
	if !d.open[idx] {
		return nil
	}
	delete(d.open, idx)
	return []ir.Event{{Type: ir.EvBlockStop, Index: idx}}
}

func (d *streamDecoder) closePart(k partKey) []ir.Event {
	idx, ok := d.parts[k]
	if !ok {
		return nil
	}
	return d.close(idx)
}

// closeItem 按开块顺序关掉一个 output item 下的全部 part，并释放其寻址状态。
func (d *streamDecoder) closeItem(out int) []ir.Event {
	var evs []ir.Event
	for _, idx := range d.itemParts[out] {
		evs = append(evs, d.close(idx)...)
	}
	delete(d.itemParts, out)
	for k := range d.parts {
		if k.out == out {
			delete(d.parts, k)
		}
	}
	return evs
}

// deref 索引缺省按 0 读：Responses 的 content_index 在单 part 消息上常被上游省略。
func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// idx 编码侧用：把索引写进 wire（含 0）。
func idx(v int) *int { return &v }

func thinkingBlock() *ir.Block {
	return &ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{}}
}

// toolBlock 工具块。it 为 nil 时只有增量帧可用，call_id/name 拿不到——那是上游
// 漏发 output_item.added 的畸形流，仍要开块，否则悬空的参数增量会让下游编码器
// 直接报错断流。
func toolBlock(kind ir.ToolKind, it *inputItem) *ir.Block {
	tu := &ir.ToolUse{Kind: kind}
	if it != nil {
		tu.ID, tu.Name = it.CallID, it.Name
	}
	return &ir.Block{Type: ir.BlockToolUse, ToolUse: tu}
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
	oi, ci := deref(se.OutputIndex), deref(se.ContentIndex)
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
			// 不在这里开块：正文与拒绝分属不同 content part，类型要到
			// content_part.added 才确定。提前开一个 text 块，纯拒绝消息就会多出
			// 一条空 output_text item（客户端渲染成一条空回答）。
			return nil, nil
		case "function_call", "custom_tool_call":
			d.sawToolCall = true
			kind := ir.ToolFunction
			full := se.Item.Arguments
			if se.Item.Type == "custom_tool_call" {
				kind = ir.ToolCustom
				full = se.Item.Input
			}
			i, out := d.assign(partKey{out: oi}, toolBlock(kind, se.Item))
			return append(out, d.completeToolArgs(i, full)...), nil
		case "reasoning":
			_, out := d.assign(partKey{out: oi}, thinkingBlock())
			return out, nil
		}
		return nil, nil
	case "response.content_part.added":
		// part 类型只在这一帧给出。refusal 与 output_text 是两种块：并入同一条
		// 通道会让客户端把拒绝渲染成普通回答。
		if se.Part == nil {
			return nil, nil
		}
		k := partKey{out: oi, content: ci}
		typ := ir.BlockText
		if se.Part.Type == "refusal" {
			typ, k.refusal = ir.BlockRefusal, true
		}
		_, out := d.assign(k, &ir.Block{Type: typ})
		return out, nil
	case "response.output_text.delta":
		if se.Delta == "" {
			return nil, nil
		}
		i, out := d.assign(partKey{out: oi, content: ci}, &ir.Block{Type: ir.BlockText})
		return append(out, ir.Event{Type: ir.EvTextDelta, Index: i, Text: se.Delta}), nil
	case "response.output_text.annotation.added":
		// 该事件没有 delta 字段：正文在 annotation 之外。此前与文本增量并档，
		// 于是恒命中 delta 为空的分支被静默丢弃，引用一条都到不了客户端。
		cs := decodeAnnotations([]annotation{*orEmptyAnnotation(se.Annotation)})
		if len(cs) == 0 {
			return nil, nil
		}
		// 索引口径是「相对本 part 的正文」，必须落到 content_index 对应的那一块。
		// 并进合并块会让 start_index 指向前一个 part 的文字。
		i, out := d.assign(partKey{out: oi, content: ci}, &ir.Block{Type: ir.BlockText})
		return append(out, ir.Event{Type: ir.EvCitation, Index: i, Citations: cs}), nil
	case "response.refusal.delta":
		if se.Delta == "" {
			return nil, nil
		}
		i, out := d.assign(partKey{out: oi, content: ci, refusal: true}, &ir.Block{Type: ir.BlockRefusal})
		return append(out, ir.Event{Type: ir.EvTextDelta, Index: i, Text: se.Delta}), nil
	case "response.refusal.done":
		return d.closePart(partKey{out: oi, content: ci, refusal: true}), nil
	case "response.content_part.done":
		// part 结束就关块，不等 output_item.done：同一条 message 的多个 part 若
		// 一起延后关闭，下游会看到 start/start/stop/stop 的交叉嵌套。
		k := partKey{out: oi, content: ci}
		if se.Part != nil && se.Part.Type == "refusal" {
			k.refusal = true
		}
		return d.closePart(k), nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if se.Delta == "" {
			return nil, nil
		}
		i, out := d.assign(partKey{out: oi}, thinkingBlock())
		return append(out, ir.Event{Type: ir.EvThinkingDelta, Index: i, Text: se.Delta}), nil
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if se.Delta == "" {
			return nil, nil
		}
		kind := ir.ToolFunction
		if se.Type == "response.custom_tool_call_input.delta" {
			kind = ir.ToolCustom
		}
		i, out := d.assign(partKey{out: oi}, toolBlock(kind, nil))
		d.toolArgs[i] += se.Delta
		return append(out, ir.Event{Type: ir.EvToolInput, Index: i, Text: se.Delta}), nil
	case "response.function_call_arguments.done":
		i, out := d.assign(partKey{out: oi}, toolBlock(ir.ToolFunction, nil))
		return append(out, d.completeToolArgs(i, se.Arguments)...), nil
	case "response.custom_tool_call_input.done":
		i, out := d.assign(partKey{out: oi}, toolBlock(ir.ToolCustom, nil))
		return append(out, d.completeToolArgs(i, se.Input)...), nil
	case "response.output_item.done":
		var out []ir.Event
		if se.Item != nil {
			switch se.Item.Type {
			case "function_call", "custom_tool_call":
				kind := ir.ToolFunction
				full := se.Item.Arguments
				if se.Item.Type == "custom_tool_call" {
					kind = ir.ToolCustom
					full = se.Item.Input
				}
				i, opened := d.assign(partKey{out: oi}, toolBlock(kind, se.Item))
				out = append(out, opened...)
				out = append(out, d.completeToolArgs(i, full)...)
				delete(d.toolArgs, i)
			case "reasoning":
				// 关 thinking 块前先发 signature_delta（对齐 sub2api :688）
				if se.Item.EncryptedContent != "" {
					i, opened := d.assign(partKey{out: oi}, thinkingBlock())
					out = append(out, opened...)
					out = append(out, ir.Event{Type: ir.EvSigDelta, Index: i, Text: se.Item.EncryptedContent,
						SignatureFrom: ir.SigFrom(Name, se.Item.EncryptedContent)})
				}
			}
		}
		return append(out, d.closeItem(oi)...), nil
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
	// 未收到 part/item 终止帧就直接结束时补关全部仍开着的块：不补会让下游编码器
	// 认为块还开着，正文卡在缓冲里发不出去。按开块顺序关，避免 start/stop 交叉。
	for _, i := range d.order {
		out = append(out, d.close(i)...)
	}
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
