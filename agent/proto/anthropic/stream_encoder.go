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
	open     map[int]ir.BlockType
	toolArgs map[int][]byte
	// text 各块已下发的正文。citations_delta 的 cited_text 与字符索引只能在
	// 正文上反推，而引用总在正文之后到达，所以必须逐块累积。
	text map[int]string
	// droppedSigs 被门控掉的外族/合成签名数，Notes() 收尾时报出。
	droppedSigs int
	// droppedTier 没能下发的档位回显原值：越集、或到得太晚（message_delta
	// 没有 service_tier 槽位，chat 系上游的晚到回显送不出去）。
	droppedTier      string
	droppedAudio     bool
	badToolArgs      int
	tierSent         bool
	messageDeltaSent bool
	stopped          bool
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{open: map[int]ir.BlockType{}, toolArgs: map[int][]byte{}, text: map[int]string{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvMessageStart:
		em := &eventMessage{ID: ev.MessageID, Model: ev.Model, Usage: encodeUsagePtr(ev.Usage)}
		if ev.ServiceTier != "" {
			if tier, ok := proto.MapServiceTierEcho(ev.ServiceTier, Name); ok {
				em.ServiceTier = tier
				e.tierSent = true
			} else {
				e.droppedTier = ev.ServiceTier
			}
		}
		// container 是本家维度，直接下发。
		em.Container = encodeContainerInfo(ev.Container)
		if ev.Audio != nil {
			e.droppedAudio = true
		}
		return [][]byte{sseFrame("message_start", marshal(streamEvent{
			Type:    "message_start",
			Message: em,
		}))}, nil
	case ir.EvBlockStart:
		e.open[ev.Index] = blockTypeOf(ev.Block)
		if e.open[ev.Index] == ir.BlockToolUse {
			e.toolArgs[ev.Index] = nil
		}
		return [][]byte{e.blockStartFrame(ev.Index, ev.Block)}, nil
	case ir.EvTextDelta:
		frames := e.ensureOpen(ev.Index, ir.BlockText)
		e.text[ev.Index] += ev.Text
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "text_delta", Text: ev.Text})), nil
	case ir.EvCitation:
		// 不调 ensureOpen：引用不是正文，凭它开一个新块会在客户端多出一个空
		// 文本块，而引用本身要贴的那段正文根本不在里面。
		if _, ok := e.open[ev.Index]; !ok {
			return nil, nil
		}
		var frames [][]byte
		for _, c := range encodeCitations(e.text[ev.Index], ev.Citations) {
			cc := c
			frames = append(frames, e.deltaFrame(ev.Index, delta{Type: "citations_delta", Citation: &cc}))
		}
		return frames, nil
	case ir.EvThinkingDelta:
		frames := e.ensureOpen(ev.Index, ir.BlockThinking)
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "thinking_delta", Thinking: ev.Text})), nil
	case ir.EvSigDelta:
		// 只转本族真签名。外族/合成签名下发到 signature 位就是冒充：客户端
		// 会在下一轮原样回传，Anthropic 的签名校验必拒整个请求。
		if ev.SignatureFrom != Name {
			e.droppedSigs++
			return nil, nil
		}
		frames := e.ensureOpen(ev.Index, ir.BlockThinking)
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "signature_delta", Signature: ev.Text})), nil
	case ir.EvToolInput:
		frames := e.ensureOpen(ev.Index, ir.BlockToolUse)
		if _, ok := e.toolArgs[ev.Index]; !ok {
			e.toolArgs[ev.Index] = nil
		}
		e.toolArgs[ev.Index] = append(e.toolArgs[ev.Index], ev.Text...)
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "input_json_delta", PartialJSON: ev.Text})), nil
	case ir.EvBlockStop:
		e.finishToolArgs(ev.Index)
		if !e.closeBlock(ev.Index) {
			return nil, nil // 未打开的 block，忽略
		}
		return [][]byte{sseFrame("content_block_stop", marshal(streamEvent{Type: "content_block_stop", Index: ev.Index}))}, nil
	case ir.EvMessageDelta:
		e.messageDeltaSent = true
		// message_delta 没有 service_tier 槽位：晚到的回显（chat 系上游的
		// 后续 chunk 才带）即便值集装得下也送不出去，照实报出。
		if ev.ServiceTier != "" && !e.tierSent && e.droppedTier == "" {
			e.droppedTier = ev.ServiceTier
		}
		return [][]byte{sseFrame("message_delta", marshal(streamEvent{
			Type:  "message_delta",
			Delta: &delta{StopReason: UnmapStopReason(ev.StopReason), StopSequence: ev.StopSequence, Container: encodeContainerInfo(ev.Container)},
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
		e.finishToolArgs(i)
		out = append(out, sseFrame("content_block_stop", marshal(streamEvent{Type: "content_block_stop", Index: i})))
		delete(e.open, i)
	}
	if !e.messageDeltaSent {
		// 走到这里说明上游没给出终止事件（EvMessageDelta 会置位）。按中断档
		// 收尾，而不是伪造 end_turn 让客户端以为模型说完了。
		out = append(out, sseFrame("message_delta", marshal(streamEvent{
			Type:  "message_delta",
			Delta: &delta{StopReason: UnmapStopReason(ir.StopAborted)},
		})))
		e.messageDeltaSent = true
	}
	if !e.stopped {
		out = append(out, sseFrame("message_stop", marshal(streamEvent{Type: "message_stop"})))
		e.stopped = true
	}
	return out
}

// Notes 排干损耗注记（被门控的外族/合成签名、没送出去的档位回显）。
func (e *streamEncoder) Notes() []string {
	var notes []string
	if e.droppedSigs > 0 {
		notes = append(notes, proto.SigDropNote(e.droppedSigs, false))
		e.droppedSigs = 0
	}
	if e.droppedTier != "" {
		notes = append(notes, proto.TierEchoDropNote(e.droppedTier))
		e.droppedTier = ""
	}
	if e.droppedAudio {
		notes = append(notes, proto.AudioOutputDropNote())
		e.droppedAudio = false
	}
	if e.badToolArgs > 0 {
		notes = append(notes, ir.RawArgsPassNote(e.badToolArgs))
		e.badToolArgs = 0
	}
	return notes
}

func (e *streamEncoder) finishToolArgs(index int) {
	raw, ok := e.toolArgs[index]
	if !ok {
		return
	}
	delete(e.toolArgs, index)
	if _, valid := ir.NormalizeToolInput(raw); !valid {
		e.badToolArgs++
	}
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
		ID:           r.ID,
		Model:        r.Model,
		Content:      decodeBlocks(r.Content),
		StopReason:   MapStopReason(r.StopReason),
		StopSequence: r.StopSequence,
		Usage:        convUsage(r.Usage),
		ServiceTier:  r.ServiceTier,
		Container:    decodeContainer(r.Container),
	}, nil
}

func (codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	out := response{
		ID:           resp.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        resp.Model,
		Content:      encodeBlocks(resp.Content),
		StopReason:   UnmapStopReason(resp.StopReason),
		StopSequence: resp.StopSequence,
		Usage: usage{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationTokens,
		},
	}
	// 值集装不下的回显（OpenAI 的 flex/fast 等）丢弃，由 ResponseNotes 报出。
	if tier, ok := proto.MapServiceTierEcho(resp.ServiceTier, Name); ok {
		out.ServiceTier = tier
	}
	out.Container = encodeContainerInfo(resp.Container)
	return json.Marshal(out)
}

// ResponseNotes 非流式编码损耗扫描：外族签名丢弃 + 对象槽位的畸形参数挪键。
func (codec) ResponseNotes(resp *ir.Response) []string {
	return proto.ScanResponseLosses(resp, Name, false, true)
}
