package openaichat

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// streamEncoder IR 事件 -> OpenAI chunk 流。
// 块序号 -> 工具稠密索引重映射；message_start -> role chunk；
// message_delta -> finish chunk + 独立 usage chunk；message_stop -> [DONE]。
// server_tool_use / web_search_tool_result 块无 OpenAI 对应形态，
// 跳过（内容经其后的摘要文本块送达）。
type streamEncoder struct {
	id, model string
	created   int64
	toolIdx   map[int]int  // block index -> dense tool index
	skipIdx   map[int]bool // server_tool_use 等无形态块（input delta 丢弃）
	nextTool  int
	stopped   bool
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{created: time.Now().Unix(), toolIdx: map[int]int{}, skipIdx: map[int]bool{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvMessageStart:
		if ev.MessageID != "" {
			e.id = ev.MessageID
		}
		if ev.Model != "" {
			e.model = ev.Model
		}
		return [][]byte{e.chunk(&message{Role: "assistant"}, "")}, nil
	case ir.EvBlockStart:
		if ev.Block != nil && ev.Block.Type == ir.BlockToolUse && ev.Block.ToolUse != nil {
			idx := e.nextTool
			e.nextTool++
			e.toolIdx[ev.Index] = idx
			return [][]byte{e.chunk(&message{ToolCalls: []toolCall{{
				Index:    idx,
				ID:       ev.Block.ToolUse.ID,
				Type:     "function",
				Function: functionCall{Name: ev.Block.ToolUse.Name, Arguments: ""},
			}}}, "")}, nil
		}
		if ev.Block != nil && ev.Block.Type != ir.BlockText && ev.Block.Type != ir.BlockThinking {
			e.skipIdx[ev.Index] = true // server_tool_use / web_search_tool_result
		}
		return nil, nil // text/thinking 块开始无需输出
	case ir.EvTextDelta:
		return [][]byte{e.chunk(&message{Content: json.RawMessage(marshalString(ev.Text))}, "")}, nil
	case ir.EvThinkingDelta:
		return [][]byte{e.chunk(&message{ReasoningContent: ev.Text}, "")}, nil
	case ir.EvSigDelta:
		return nil, nil // OpenAI 无签名概念，丢弃
	case ir.EvToolInput:
		if e.skipIdx[ev.Index] {
			return nil, nil // 服务端工具块参数无 OpenAI 形态
		}
		idx, ok := e.toolIdx[ev.Index]
		if !ok {
			return nil, fmt.Errorf("openai-chat: tool input for unopened block %d", ev.Index)
		}
		return [][]byte{e.chunk(&message{ToolCalls: []toolCall{{
			Index:    idx,
			Type:     "function",
			Function: functionCall{Arguments: ev.Text},
		}}}, "")}, nil
	case ir.EvBlockStop:
		return nil, nil // OpenAI 无块结束帧
	case ir.EvMessageDelta:
		var frames [][]byte
		frames = append(frames, e.chunk(&message{}, UnmapFinishReason(ev.StopReason)))
		if ev.Usage != nil {
			frames = append(frames, e.usageChunk(ev.Usage))
		}
		return frames, nil
	case ir.EvMessageStop:
		e.stopped = true
		return [][]byte{[]byte("data: [DONE]\n\n")}, nil
	case ir.EvPing:
		return nil, nil
	case ir.EvError:
		return [][]byte{New().RenderStreamError(ev.Err)}, nil
	}
	return nil, fmt.Errorf("openai-chat: encode unknown event %q", ev.Type)
}

// Finish 兜底：若流未正常结束，补 finish chunk + [DONE]。
func (e *streamEncoder) Finish() [][]byte {
	if e.stopped {
		return nil
	}
	e.stopped = true
	return [][]byte{
		e.chunk(&message{}, "stop"),
		[]byte("data: [DONE]\n\n"),
	}
}

func (e *streamEncoder) chunk(delta *message, finishReason string) []byte {
	return []byte("data: " + string(marshal(response{
		ID:      e.id,
		Object:  "chat.completion.chunk",
		Created: e.created,
		Model:   e.model,
		Choices: []choice{{Index: 0, Delta: delta, FinishReason: finishReason}},
	})) + "\n\n")
}

func (e *streamEncoder) usageChunk(u *ir.Usage) []byte {
	return []byte("data: " + string(marshal(response{
		ID:      e.id,
		Object:  "chat.completion.chunk",
		Created: e.created,
		Model:   e.model,
		Choices: []choice{},
		Usage:   encodeUsage(u),
	})) + "\n\n")
}

// encodeUsage IR -> OpenAI（prompt 含 cached 的总输入口径）。
func encodeUsage(u *ir.Usage) *usage {
	if u == nil {
		return nil
	}
	out := &usage{
		PromptTokens:     u.TotalInput(),
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalInput() + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out.PromptTokensDetails = &promptDetails{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &completionDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// ---- 非流式响应 ----

func (codec) DecodeResponse(body []byte) (*ir.Response, error) {
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai-chat: decode response: %w", err)
	}
	out := &ir.Response{ID: r.ID, Model: r.Model}
	if len(r.Choices) > 0 && r.Choices[0].Message != nil {
		m := r.Choices[0].Message
		if m.ReasoningContent != "" {
			out.Content = append(out.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: m.ReasoningContent}})
		}
		out.Content = append(out.Content, contentBlocks(m.Content)...)
		for _, tc := range m.ToolCalls {
			out.Content = append(out.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			}})
		}
		out.StopReason = MapFinishReason(r.Choices[0].FinishReason)
	}
	if r.Usage != nil {
		out.Usage = decodeUsage(r.Usage)
	}
	return out, nil
}

func (codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	msg := &message{Role: "assistant"}
	var text string
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockText:
			text += b.Text
		case ir.BlockThinking:
			if b.Thinking != nil {
				msg.ReasoningContent += b.Thinking.Text
			}
		case ir.BlockToolUse:
			if b.ToolUse != nil {
				args := string(b.ToolUse.Input)
				if args == "" {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, toolCall{
					Index:    len(msg.ToolCalls),
					ID:       b.ToolUse.ID,
					Type:     "function",
					Function: functionCall{Name: b.ToolUse.Name, Arguments: args},
				})
			}
		}
	}
	if text != "" {
		msg.Content = json.RawMessage(marshalString(text))
	}
	return json.Marshal(response{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []choice{{Index: 0, Message: msg, FinishReason: UnmapFinishReason(resp.StopReason)}},
		Usage:   encodeUsage(&resp.Usage),
	})
}
