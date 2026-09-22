package gemini

import (
	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// streamEncoder IR 事件 -> Gemini SSE（data: generateResponse chunk）。
// Gemini 没有 message_start 类事件，流就是一串 chunk；
// functionCall 不增量传 args，因此 tool_use 块攒到 block_stop 一次性发出。
type streamEncoder struct {
	model string
	id    string

	pendingTool map[int]*encTool // block index -> 构建中的 functionCall
	// texts 各正文块已下发的增量与各自占用的 part 序号。Gemini 的
	// groundingSupports 区间相对**拼接后**的 part 正文、partIndex 指全局部件
	// 序号，所以必须逐增量记录——partIndex 硬编码 0 会把多块/思考在前的
	// 引用指到别的 part 上。
	texts     map[int]*encText
	partCount int
	// droppedSigs 被门控的外族/合成签名数；rewrappedArgs 畸形工具参数挪键数。
	// 两者由 Notes() 收尾时报出。
	droppedSigs   int
	rewrappedArgs int
	// droppedTier 没送出去的档位回显原值：Gemini 响应没有该槽位，恒丢。
	droppedTier string
	finished    bool
}

// encText 一个正文块的流式下发记录。
type encText struct {
	deltas []string // 逐增量正文（每个增量在客户端拼接后是一个 part）
	parts  []int    // 每个增量对应的全局 part 序号
}

type encTool struct {
	name string
	id   string
	args []byte
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{pendingTool: map[int]*encTool{}, texts: map[int]*encText{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvMessageStart:
		e.model = ev.Model
		e.id = ev.MessageID
		if ev.ServiceTier != "" {
			e.droppedTier = ev.ServiceTier
		}
		return nil, nil
	case ir.EvTextDelta:
		t := e.texts[ev.Index]
		if t == nil {
			t = &encText{}
			e.texts[ev.Index] = t
		}
		t.deltas = append(t.deltas, ev.Text)
		t.parts = append(t.parts, e.partCount)
		e.partCount++
		return e.chunk([]part{{Text: ev.Text}}, ""), nil
	case ir.EvCitation:
		// 单独一个 chunk 承载 groundingMetadata（Gemini 原生也是在正文 chunk
		// 之后的独立 chunk 里下发）。parts 留空：重发正文会让客户端看到重复文字。
		gm := encodeGroundingStreamed(e.texts[ev.Index], ev.Citations)
		if gm == nil {
			return nil, nil
		}
		return [][]byte{sseFrame(marshal(generateResponse{
			Candidates:   []candidate{{Content: &content{Role: "model", Parts: []part{}}, GroundingMetadata: gm}},
			ModelVersion: e.model,
			ResponseID:   e.id,
		}))}, nil
	case ir.EvThinkingDelta:
		e.partCount++ // thought part 也占拼接后的部件序号
		return e.chunk([]part{{Text: ev.Text, Thought: true}}, ""), nil
	case ir.EvSigDelta:
		// thoughtSignature 作为独立 part 下发（Gemini 原生也是如此：
		// 签名在思考文本结束后的单独 part 中到达）。只下发本族真签名：
		// 外族/合成签名占了这一格，客户端下一轮回传必被 Gemini 拒。
		if ev.SignatureFrom != Name {
			e.droppedSigs++
			return nil, nil
		}
		e.partCount++ // 签名 part 同样占部件序号
		return e.chunk([]part{{ThoughtSignature: ev.Text}}, ""), nil
	case ir.EvBlockStart:
		if ev.Block != nil && ev.Block.Type == ir.BlockToolUse && ev.Block.ToolUse != nil {
			e.pendingTool[ev.Index] = &encTool{name: ev.Block.ToolUse.Name, id: ev.Block.ToolUse.ID}
		}
		return nil, nil
	case ir.EvToolInput:
		if t := e.pendingTool[ev.Index]; t != nil {
			t.args = append(t.args, ev.Text...)
		}
		return nil, nil
	case ir.EvBlockStop:
		if t := e.pendingTool[ev.Index]; t != nil {
			delete(e.pendingTool, ev.Index)
			// 截断/非对象参数放进 RawMessage 槽位会让这个 chunk marshal
			// 失败，整块 functionCall 丢失；原文挪进 RawArgsKey 保真。
			args, ok := ir.NormalizeToolInput(t.args)
			if !ok {
				e.rewrappedArgs++
			}
			p := content{Role: "model", Parts: []part{{FunctionCall: &functionCall{Name: t.name, Args: args, ID: t.id}}}}
			ensureThoughtSignature(&p)
			e.partCount++ // functionCall part 也占部件序号
			return e.chunk(p.Parts, ""), nil
		}
		return nil, nil
	case ir.EvMessageDelta:
		e.finished = true
		if ev.ServiceTier != "" && e.droppedTier == "" {
			e.droppedTier = ev.ServiceTier
		}
		return e.finishChunk(ev.StopReason, ev.Usage), nil
	case ir.EvMessageStop, ir.EvPing:
		return nil, nil
	case ir.EvError:
		e.finished = true
		c := codec{}
		return [][]byte{c.RenderStreamError(ev.Err)}, nil
	}
	return nil, nil
}

// chunk 生成一个仅含内容部件的 chunk。
func (e *streamEncoder) chunk(parts []part, finishReason string) [][]byte {
	return [][]byte{sseFrame(marshal(generateResponse{
		Candidates:   []candidate{{Content: &content{Role: "model", Parts: parts}, FinishReason: finishReason}},
		ModelVersion: e.model,
		ResponseID:   e.id,
	}))}
}

// finishChunk 终止 chunk：空 parts + finishReason + usageMetadata。
func (e *streamEncoder) finishChunk(stop ir.StopReason, u *ir.Usage) [][]byte {
	resp := generateResponse{
		Candidates:   []candidate{{Content: &content{Role: "model", Parts: []part{}}, FinishReason: UnmapFinishReason(stop)}},
		ModelVersion: e.model,
		ResponseID:   e.id,
	}
	if u != nil {
		resp.UsageMetadata = encodeUsage(*u)
	}
	return [][]byte{sseFrame(marshal(resp))}
}

// Finish 冲刷未完结的 functionCall；正常结束后无残余。
func (e *streamEncoder) Finish() [][]byte {
	var out [][]byte
	for idx, t := range e.pendingTool {
		delete(e.pendingTool, idx)
		args, ok := ir.NormalizeToolInput(t.args)
		if !ok {
			e.rewrappedArgs++
		}
		p := content{Role: "model", Parts: []part{{FunctionCall: &functionCall{Name: t.name, Args: args, ID: t.id}}}}
		ensureThoughtSignature(&p)
		e.partCount++
		out = append(out, e.chunk(p.Parts, "")...)
	}
	if !e.finished {
		e.finished = true
		// 上游没给终止 chunk 就断了：按中断档收尾，不伪造 STOP。
		out = append(out, e.finishChunk(ir.StopAborted, nil)...)
	}
	return out
}

// Notes 排干损耗注记（被门控的签名 + 被挪键的畸形工具参数）。
func (e *streamEncoder) Notes() []string {
	var notes []string
	if e.droppedSigs > 0 {
		notes = append(notes, proto.SigDropNote(e.droppedSigs, false))
		e.droppedSigs = 0
	}
	if e.rewrappedArgs > 0 {
		notes = append(notes, ir.RewrapNote(e.rewrappedArgs))
		e.rewrappedArgs = 0
	}
	if e.droppedTier != "" {
		notes = append(notes, proto.TierEchoDropNote(e.droppedTier))
		e.droppedTier = ""
	}
	return notes
}
