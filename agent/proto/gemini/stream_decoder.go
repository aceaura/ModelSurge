package gemini

import (
	"encoding/json"
	"fmt"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// streamDecoder Gemini SSE（alt=sse，每个 data 是一个 generateResponse chunk）
// -> IR 事件。Gemini 的 functionCall 一次性完整到达（args 不是增量），
// 因此一个 functionCall 直接产出 block_start + input_json_delta + block_stop 三事件。
type streamDecoder struct {
	started  bool
	finished bool
	model    string
	id       string

	openIdx  int          // 当前打开的内容块序号；-1 表示无
	openType ir.BlockType // 当前打开的块类型
	nextIdx  int

	callSeq int
	sawTool bool
	stop    ir.StopReason
	hasStop bool
	usage   *ir.Usage
}

func (codec) NewStreamDecoder() proto.StreamDecoder { return &streamDecoder{openIdx: -1} }

func (d *streamDecoder) Feed(event, data string) ([]ir.Event, error) {
	if data == "[DONE]" || data == "" {
		return nil, nil
	}
	var chunk generateResponse
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, fmt.Errorf("gemini: decode stream chunk: %w", err)
	}
	var out []ir.Event
	if !d.started {
		d.started = true
		d.model = chunk.ModelVersion
		d.id = chunk.ResponseID
		out = append(out, ir.Event{Type: ir.EvMessageStart, MessageID: d.id, Model: d.model})
	}
	if c := firstCandidate(&chunk); c != nil {
		if c.Content != nil {
			for _, p := range c.Content.Parts {
				out = append(out, d.feedPart(p)...)
			}
		}
		if c.FinishReason != "" {
			d.stop = MapFinishReason(c.FinishReason, d.sawTool)
			d.hasStop = true
		}
	}
	if u := decodeUsage(chunk.UsageMetadata); u != nil {
		d.usage = u
	}
	// finishReason 与 usage 通常在最后一个 chunk 一起到达
	if d.hasStop {
		out = append(out, d.terminate()...)
	}
	return out, nil
}

func firstCandidate(r *generateResponse) *candidate {
	for i := range r.Candidates {
		if r.Candidates[i].Index == 0 {
			return &r.Candidates[i]
		}
	}
	if len(r.Candidates) > 0 {
		return &r.Candidates[0]
	}
	return nil
}

func (d *streamDecoder) feedPart(p part) []ir.Event {
	if p.FunctionCall != nil {
		d.sawTool = true
		d.callSeq++
		args := string(p.FunctionCall.Args)
		if args == "" {
			args = "{}"
		}
		idx := d.nextIdx
		d.nextIdx++
		id := p.FunctionCall.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", d.callSeq)
		}
		return append(d.closeOpen(),
			ir.Event{Type: ir.EvBlockStart, Index: idx, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID: id, Name: p.FunctionCall.Name,
			}}},
			ir.Event{Type: ir.EvToolInput, Index: idx, Text: args},
			ir.Event{Type: ir.EvBlockStop, Index: idx},
		)
	}
	var typ ir.BlockType
	if p.Thought || p.ThoughtSignature != "" {
		typ = ir.BlockThinking
	} else if p.Text != "" {
		typ = ir.BlockText
	} else {
		return nil
	}
	var out []ir.Event
	if d.openIdx < 0 || d.openType != typ {
		out = append(out, d.closeOpen()...)
		d.openIdx = d.nextIdx
		d.nextIdx++
		d.openType = typ
		blk := &ir.Block{Type: typ}
		if typ == ir.BlockThinking {
			blk.Thinking = &ir.Thinking{}
		}
		out = append(out, ir.Event{Type: ir.EvBlockStart, Index: d.openIdx, Block: blk})
	}
	if p.Text != "" {
		evType := ir.EvTextDelta
		if typ == ir.BlockThinking {
			evType = ir.EvThinkingDelta
		}
		out = append(out, ir.Event{Type: evType, Index: d.openIdx, Text: p.Text})
	}
	if p.ThoughtSignature != "" {
		out = append(out, ir.Event{Type: ir.EvSigDelta, Index: d.openIdx, Text: p.ThoughtSignature})
	}
	return out
}

// closeOpen 关闭当前打开的块。
func (d *streamDecoder) closeOpen() []ir.Event {
	if d.openIdx < 0 {
		return nil
	}
	out := []ir.Event{{Type: ir.EvBlockStop, Index: d.openIdx}}
	d.openIdx = -1
	return out
}

// terminate 产出收尾事件序列（message_delta + message_stop），幂等。
func (d *streamDecoder) terminate() []ir.Event {
	if d.finished {
		return nil
	}
	d.finished = true
	stop := d.stop
	if stop == "" {
		stop = MapFinishReason("", d.sawTool)
	}
	out := d.closeOpen()
	md := ir.Event{Type: ir.EvMessageDelta, StopReason: stop}
	if d.usage != nil {
		md.Usage = d.usage
	}
	return append(out, md, ir.Event{Type: ir.EvMessageStop})
}

// Finish 异常断流兜底：补收尾事件。正常结束后调用返回空。
func (d *streamDecoder) Finish() []ir.Event {
	if !d.started {
		return nil
	}
	return d.terminate()
}
