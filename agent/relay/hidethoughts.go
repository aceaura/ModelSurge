package relay

import (
	"fmt"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// hidethoughts.go 客户端要求不回显思考内容时的抑制层。
//
// 只有 Gemini 的 thinkingConfig.includeThoughts=false 能表达这一诉求，而它
// 无法下压给上游：四个出站协议都没有「照常思考但别返回思考」的字段，上游
// 不会因此少想。所以抑制只能发生在回客户端的方向上。
//
// 实现为 InboundCodec 的装饰器而不是给五处写出点各传一个 flag：客户端写出
// 有五个出口（stream/collect × remote/kiro，加 writeResponse），逐处判断会
// 漏，且每处的事件形态不同。装饰一次，所有出口自动覆盖。

// withHiddenThoughts 在客户端要求隐藏思考时包一层抑制装饰器；否则原样返回。
func withHiddenThoughts(c proto.InboundCodec, req *ir.Request) proto.InboundCodec {
	if req == nil || req.Thinking == nil || !req.Thinking.HideThoughts {
		return c
	}
	return hideThoughtsCodec{InboundCodec: c}
}

type hideThoughtsCodec struct{ proto.InboundCodec }

func (h hideThoughtsCodec) NewStreamEncoder() proto.StreamEncoder {
	return &hideThoughtsEncoder{inner: h.InboundCodec.NewStreamEncoder()}
}

func (h hideThoughtsCodec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	return h.InboundCodec.EncodeResponse(stripThinking(resp))
}

func (h hideThoughtsCodec) ResponseNotes(resp *ir.Response) []string {
	stripped, n := stripThinkingCount(resp)
	notes := h.InboundCodec.ResponseNotes(stripped)
	if n > 0 {
		notes = append([]string{hideNote(n)}, notes...)
	}
	return notes
}

// hideNote 思考抑制注记：上游照常思考照常计费，只是没回显。
func hideNote(n int) string {
	return fmt.Sprintf("suppressed %d thinking block(s): the client asked to hide thoughts, but the upstream still thought and billed for them", n)
}

// hideThoughtsEncoder 丢弃思考类事件。thinking 块的 start/stop 一并吞掉，
// 否则下游编码器会看到一个空块（Anthropic 客户端会因缺 signature 报错）。
type hideThoughtsEncoder struct {
	inner  proto.StreamEncoder
	hidden map[int]bool
	// suppressed 吞掉的 thinking 块数，Notes() 收尾时报出。
	suppressed int
}

func (e *hideThoughtsEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvThinkingDelta, ir.EvSigDelta:
		return nil, nil
	case ir.EvBlockStart:
		if ev.Block != nil && ev.Block.Type == ir.BlockThinking {
			if e.hidden == nil {
				e.hidden = map[int]bool{}
			}
			e.hidden[ev.Index] = true
			e.suppressed++
			return nil, nil
		}
	case ir.EvBlockStop:
		if e.hidden[ev.Index] {
			delete(e.hidden, ev.Index)
			return nil, nil
		}
	}
	return e.inner.Encode(ev)
}

func (e *hideThoughtsEncoder) Finish() [][]byte { return e.inner.Finish() }

// Notes 自己的抑制注记在前，内层编码器的损耗注记在后。
func (e *hideThoughtsEncoder) Notes() []string {
	var notes []string
	if e.suppressed > 0 {
		notes = append(notes, hideNote(e.suppressed))
	}
	return append(notes, e.inner.Notes()...)
}

// stripThinking 去掉响应里的思考块。就地改会污染调用方持有的聚合响应
// （估算 usage、日志摘要都还在读它），所以浅拷一层。
func stripThinking(resp *ir.Response) *ir.Response {
	out, _ := stripThinkingCount(resp)
	return out
}

func stripThinkingCount(resp *ir.Response) (*ir.Response, int) {
	if resp == nil {
		return nil, 0
	}
	kept := make([]ir.Block, 0, len(resp.Content))
	for _, b := range resp.Content {
		if b.Type == ir.BlockThinking {
			continue
		}
		kept = append(kept, b)
	}
	if len(kept) == len(resp.Content) {
		return resp, 0
	}
	out := *resp
	out.Content = kept
	return &out, len(resp.Content) - len(kept)
}
