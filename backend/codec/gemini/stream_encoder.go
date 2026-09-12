package gemini

import (
	"encoding/json"

	proto "relayd/backend/codec"
	"relayd/backend/ir"
)

// streamEncoder IR 事件 -> Gemini SSE（data: generateResponse chunk）。
// Gemini 没有 message_start 类事件，流就是一串 chunk；
// functionCall 不增量传 args，因此 tool_use 块攒到 block_stop 一次性发出。
type streamEncoder struct {
	model string
	id    string

	pendingTool map[int]*encTool // block index -> 构建中的 functionCall
	finished    bool
}

type encTool struct {
	name string
	id   string
	args []byte
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{pendingTool: map[int]*encTool{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvMessageStart:
		e.model = ev.Model
		e.id = ev.MessageID
		return nil, nil
	case ir.EvTextDelta:
		return e.chunk([]part{{Text: ev.Text}}, ""), nil
	case ir.EvThinkingDelta:
		return e.chunk([]part{{Text: ev.Text, Thought: true}}, ""), nil
	case ir.EvSigDelta:
		// thoughtSignature 作为独立 part 下发（Gemini 原生也是如此：
		// 签名在思考文本结束后的单独 part 中到达）
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
			args := json.RawMessage(t.args)
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			return e.chunk([]part{{FunctionCall: &functionCall{Name: t.name, Args: args, ID: t.id}}}, ""), nil
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
		args := json.RawMessage(t.args)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		out = append(out, e.chunk([]part{{FunctionCall: &functionCall{Name: t.name, Args: args, ID: t.id}}}, "")...)
	}
	if !e.finished {
		e.finished = true
		out = append(out, e.finishChunk(ir.StopEndTurn, nil)...)
	}
	return out
}
