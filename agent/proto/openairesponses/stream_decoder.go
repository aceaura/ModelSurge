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
	// sawError 已下发过 EvError。上游的真实序列是 error 之后再跟终止帧
	// （sub2api 的 OpenAI 抓包夹具是 error -> response.failed）；不置这个标记，
	// 后到的 response.completed 会照常产出 StopEndTurn 的终止事件，把流内错误
	// 伪装成正常结束，response.failed 则会再发一遍同样的错误。
	sawError bool
	// tier 档位回显：response.created 与 completed/incomplete 都可能携带，
	// 后者晚到时随终止的 message_delta 事件交付。
	tier string
	// toolArgs 记录已发出的参数前缀（按 IR 块序号）。done 事件携带完整值时只补
	// 缺失后缀，既覆盖 done-only 上游，又不把已有 delta 重复一遍。
	toolArgs map[int]string
	// blockText 已发出的正文/思考前缀（按 IR 块序号）。判据同 toolArgs：done
	// 事件带的是完整值而不是新一份内容，只补缺失后缀。
	blockText map[int]string
	// cites 已发出的引用条数（按 IR 块序号）。done 事件里的 annotations 是全量
	// 快照，只补 annotation.added 没给过的那几条（对齐 new-api AnnotationCount）。
	cites map[int]int
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
		blockText: map[int]string{},
		cites:     map[int]int{},
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

// backfill 用 part 级 done 事件携带的完整值补齐缺口，块尚未开时补开。
// 块已关就不动：再发增量会让下游编码器把内容追加到已定稿的 item 上
// （对齐 new-api mergeFinalValue 的 block.Stopped 判据）。
func (d *streamDecoder) backfill(k partKey, blk *ir.Block, full string, typ ir.EventType) []ir.Event {
	if full == "" {
		return nil
	}
	i, out := d.assign(k, blk)
	if !d.open[i] {
		return nil
	}
	return append(out, completeValue(d.blockText, i, full, typ)...)
}

// backfillCites 补 done 事件里的全量引用快照，只发 annotation.added 没给过的部分。
// 快照口径与增量口径都是「相对本 part 的正文」，落在同一块上索引才不错位。
func (d *streamDecoder) backfillCites(k partKey, as []annotation) []ir.Event {
	if len(as) == 0 {
		return nil
	}
	i, out := d.assign(k, &ir.Block{Type: ir.BlockText})
	if !d.open[i] || len(as) <= d.cites[i] {
		return out
	}
	cs := decodeAnnotations(as[d.cites[i]:])
	d.cites[i] = len(as)
	if len(cs) == 0 {
		return out
	}
	return append(out, ir.Event{Type: ir.EvCitation, Index: i, Citations: cs})
}

// completeItemParts 从 output_item.done 的 message content 回补正文与引用：
// done-only 上游的整条正文只在这里出现，漏读等于一个字都到不了客户端。
// part 在数组里的位置就是它的 content_index。
func (d *streamDecoder) completeItemParts(oi int, raw json.RawMessage) []ir.Event {
	if len(raw) == 0 {
		return nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []ir.Event
	for n, p := range parts {
		k := partKey{out: oi, content: n}
		if p.Type == "refusal" {
			// refusal 位必须置上：漏了就会与流式路径开的那块错开成两块，
			// 同一段拒绝正文被下发两遍。
			k.refusal = true
			out = append(out, d.backfill(k, &ir.Block{Type: ir.BlockRefusal}, p.Refusal, ir.EvTextDelta)...)
			continue
		}
		if p.Type != "" && p.Type != "output_text" {
			continue
		}
		out = append(out, d.backfill(k, &ir.Block{Type: ir.BlockText}, p.Text, ir.EvTextDelta)...)
		out = append(out, d.backfillCites(k, p.Annotations)...)
	}
	return out
}

// reasoningSummaryText 拼接 reasoning item 的 summary_text（对齐 new-api
// reasoningOutputText：有正文用正文，否则退回 summary）。
func reasoningSummaryText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var parts []summaryPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

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
		d.blockText[i] += se.Delta
		return append(out, ir.Event{Type: ir.EvTextDelta, Index: i, Text: se.Delta}), nil
	case "response.output_text.done":
		// 完整正文与全量引用都在这一帧。此前整个事件落到 default 被丢掉，
		// 只发终态不发增量的上游整段正文一个字都到不了客户端。
		k := partKey{out: oi, content: ci}
		out := d.backfill(k, &ir.Block{Type: ir.BlockText}, se.Text, ir.EvTextDelta)
		return append(out, d.backfillCites(k, se.Annotations)...), nil
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
		d.cites[i] += len(cs)
		return append(out, ir.Event{Type: ir.EvCitation, Index: i, Citations: cs}), nil
	case "response.refusal.delta":
		if se.Delta == "" {
			return nil, nil
		}
		i, out := d.assign(partKey{out: oi, content: ci, refusal: true}, &ir.Block{Type: ir.BlockRefusal})
		d.blockText[i] += se.Delta
		return append(out, ir.Event{Type: ir.EvTextDelta, Index: i, Text: se.Delta}), nil
	case "response.refusal.done":
		k := partKey{out: oi, content: ci, refusal: true}
		out := d.backfill(k, &ir.Block{Type: ir.BlockRefusal}, se.Refusal, ir.EvTextDelta)
		return append(out, d.closePart(k)...), nil
	case "response.content_part.done":
		// part 结束就关块，不等 output_item.done：同一条 message 的多个 part 若
		// 一起延后关闭，下游会看到 start/start/stop/stop 的交叉嵌套。
		k := partKey{out: oi, content: ci}
		var out []ir.Event
		if se.Part != nil && se.Part.Type == "refusal" {
			k.refusal = true
			out = d.backfill(k, &ir.Block{Type: ir.BlockRefusal}, se.Part.Refusal, ir.EvTextDelta)
		} else if se.Part != nil {
			out = d.backfill(k, &ir.Block{Type: ir.BlockText}, se.Part.Text, ir.EvTextDelta)
			out = append(out, d.backfillCites(k, se.Part.Annotations)...)
		}
		return append(out, d.closePart(k)...), nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if se.Delta == "" {
			return nil, nil
		}
		i, out := d.assign(partKey{out: oi}, thinkingBlock())
		d.blockText[i] += se.Delta
		return append(out, ir.Event{Type: ir.EvThinkingDelta, Index: i, Text: se.Delta}), nil
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		// 不在这里关块：encrypted_content 要到 output_item.done 才给，提前关会丢
		// signature_delta 并打断多轮缓存（对齐 sub2api responses_to_anthropic.go:238）。
		return d.backfill(partKey{out: oi}, thinkingBlock(), se.Text, ir.EvThinkingDelta), nil
	case "response.reasoning_summary_part.done":
		if se.Part == nil {
			return nil, nil
		}
		return d.backfill(partKey{out: oi}, thinkingBlock(), se.Part.Text, ir.EvThinkingDelta), nil
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
			case "message":
				// done-only 上游的整条正文只在 item.content 里，前面一帧增量都没有。
				out = append(out, d.completeItemParts(oi, se.Item.Content)...)
			case "reasoning":
				// 关 thinking 块前先发 signature_delta（对齐 sub2api :688）
				full := reasoningSummaryText(se.Item.Summary)
				if full != "" || se.Item.EncryptedContent != "" {
					i, opened := d.assign(partKey{out: oi}, thinkingBlock())
					out = append(out, opened...)
					if d.open[i] {
						out = append(out, completeValue(d.blockText, i, full, ir.EvThinkingDelta)...)
					}
					if se.Item.EncryptedContent != "" {
						out = append(out, ir.Event{Type: ir.EvSigDelta, Index: i, Text: se.Item.EncryptedContent,
							SignatureFrom: ir.SigFrom(Name, se.Item.EncryptedContent)})
					}
				}
			}
		}
		return append(out, d.closeItem(oi)...), nil
	case "response.completed":
		d.finished = true
		if d.sawError {
			return nil, nil
		}
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
		if d.sawError {
			return nil, nil
		}
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
		if d.sawError {
			return nil, nil
		}
		d.sawError = true
		return []ir.Event{{Type: ir.EvError, Err: streamErrorOf(se, "upstream response failed")}}, nil
	case "error":
		// 终止标记必须在这里置上。真实上游的序列是 error 之后再跟终止帧，不置
		// 就会让后到的 response.completed 照常产出 StopEndTurn 的终止事件，把流内
		// 错误伪装成正常结束；response.failed 则会把同一个错误再发一遍。
		d.finished = true
		if d.sawError {
			return nil, nil
		}
		d.sawError = true
		return []ir.Event{{Type: ir.EvError, Err: streamErrorOf(se, "upstream stream error")}}, nil
	}
	return nil, nil // response.queued / in_progress 等进度事件忽略
}

// streamErrorOf 流式错误事件 -> IR 错误。裸 error 事件的错误体在顶层，
// response.failed 的在 response.error 下，两处都读（cc-switch 与 sub2api 同样
// 做这个双层回落）。
//
// 字段缺席时保留规范默认值而不是覆盖成空：type 为空会让下游 RenderStreamError
// 产出一个没有规范类型的错误帧，客户端无从判断该不该重试。
func streamErrorOf(se streamEvent, fallbackMsg string) *ir.Error {
	e := &ir.Error{Type: ir.ErrTypeUpstream, Message: fallbackMsg, Retryable: true}
	var b *errorBody
	switch {
	case se.Error != nil:
		b = se.Error
	case se.Response != nil:
		b = se.Response.Error
	}
	if b == nil {
		return e
	}
	if b.Type != "" {
		e.Type = b.Type
	}
	if b.Message != "" {
		e.Message = b.Message
	}
	e.Code = b.Code
	// 风控拦截不可重试（对齐 sub2api cyber_policy 特例）。判成可重试会让调度器
	// 换目标重发一个永远不可能成功的请求，把整个账号池白烧一遍。
	if e.Code == "cyber_policy" || b.Type == "content_filter" {
		e.Retryable = false
		e.Type = ir.ErrTypeContentFilter
	}
	return e
}

// completeToolArgs 用终态完整值补齐尚未收到的参数后缀。
func (d *streamDecoder) completeToolArgs(index int, full string) []ir.Event {
	return completeValue(d.toolArgs, index, full, ir.EvToolInput)
}

// completeValue 用终态完整值补齐尚未收到的后缀。done 不是新一份内容，已由增量
// 交付的前缀不得重复；终态比当前值短或与之分叉时也不能追加，否则拼出畸形内容。
// new-api mergeFinalValue 与 cc-switch missing_suffix 用的是同一套判据。
func completeValue(acc map[int]string, index int, full string, typ ir.EventType) []ir.Event {
	if full == "" {
		return nil
	}
	current := acc[index]
	if current == full {
		return nil
	}
	if !strings.HasPrefix(full, current) {
		return nil
	}
	acc[index] = full
	return []ir.Event{{Type: typ, Index: index, Text: strings.TrimPrefix(full, current)}}
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
