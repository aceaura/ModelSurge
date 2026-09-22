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
	// text 各块已下发的正文。Gemini 的 groundingSupports 区间是相对**累积后**
	// 的 part 正文，所以必须逐块累积才能算出区间。
	text     map[int]string
	finished bool
}

type encTool struct {
	name string
	id   string
	args []byte
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{pendingTool: map[int]*encTool{}, text: map[int]string{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvMessageStart:
		e.model = ev.Model
		e.id = ev.MessageID
		return nil, nil
	case ir.EvTextDelta:
		e.text[ev.Index] += ev.Text
		return e.chunk([]part{{Text: ev.Text}}, ""), nil
	case ir.EvCitation:
		// 单独一个 chunk 承载 groundingMetadata（Gemini 原生也是在正文 chunk
		// 之后的独立 chunk 里下发）。parts 留空：重发正文会让客户端看到重复文字。
		// partIndex 恒 0：流式 chunk 里的 parts 数组就这一格。
		gm := encodeGrounding(
			[]ir.Block{{Type: ir.BlockText, Text: e.text[ev.Index], Citations: ev.Citations}},
			map[int]int{0: 0})
		if gm == nil {
			return nil, nil
		}
		return [][]byte{sseFrame(marshal(generateResponse{
			Candidates:   []candidate{{Content: &content{Role: "model", Parts: []part{}}, GroundingMetadata: gm}},
			ModelVersion: e.model,
			ResponseID:   e.id,
		}))}, nil
	case ir.EvThinkingDelta:
		return e.chunk([]part{{Text: ev.Text, Thought: true}}, ""), nil
	case ir.EvSigDelta:
		// thoughtSignature 作为独立 part 下发（Gemini 原生也是如此：
		// 签名在思考文本结束后的单独 part 中到达）。只下发本族真签名：
		// 外族/合成签名占了这一格，客户端下一轮回传必被 Gemini 拒。
		if ev.SignatureFrom != Name {
			return nil, nil
		}
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
			args, _ := ir.NormalizeToolInput(t.args)
			p := content{Role: "model", Parts: []part{{FunctionCall: &functionCall{Name: t.name, Args: args, ID: t.id}}}}
			ensureThoughtSignature(&p)
			return e.chunk(p.Parts, ""), nil
		}
		return nil, nil
	case ir.EvMessageDelta:
		e.finished = true
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
		args, _ := ir.NormalizeToolInput(t.args)
		p := content{Role: "model", Parts: []part{{FunctionCall: &functionCall{Name: t.name, Args: args, ID: t.id}}}}
		ensureThoughtSignature(&p)
		out = append(out, e.chunk(p.Parts, "")...)
	}
	if !e.finished {
		e.finished = true
		// 上游没给终止 chunk 就断了：按中断档收尾，不伪造 STOP。
		out = append(out, e.finishChunk(ir.StopAborted, nil)...)
	}
	return out
}
