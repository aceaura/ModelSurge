package openairesponses

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// streamEncoder IR 事件 -> Responses SSE。
// 每个块累积完整内容，以便 output_item.done 与 response.completed
// 携带完整 item/output（OpenAI SDK 的 get_final_response 依赖完整 output）。
type streamEncoder struct {
	id, model string
	created   int64
	seq       int

	blocks map[int]*encBlock
	order  []int
	// skip 服务端工具块（server_tool_use / web_search_tool_result）的 index。
	// Responses 协议里没有对应 item 类型：它们是上游自己执行的搜索，客户端既
	// 不需要回传也无法回传。落进 text 分支会把查询 JSON 拼进 output_text，
	// 正文里凭空多出一段参数串——宁可不出现。
	skip map[int]bool

	stopReason ir.StopReason
	usage      *ir.Usage
	completed  bool
}

type encBlock struct {
	typ              ir.BlockType
	itemID           string
	toolID, toolName string
	text             string // text / thinking / arguments 累积
	sig              string
	closed           bool
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{created: time.Now().Unix(), blocks: map[int]*encBlock{}, skip: map[int]bool{}}
}

func (e *streamEncoder) nextID(prefix string) string {
	e.seq++
	return fmt.Sprintf("%s_%04d", prefix, e.seq)
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
		return [][]byte{e.frame(streamEvent{Type: "response.created", Response: &responseObj{
			ID: e.id, Object: "response", CreatedAt: e.created, Model: e.model, Status: "in_progress",
		}})}, nil
	case ir.EvBlockStart:
		return e.blockStart(ev)
	case ir.EvTextDelta:
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, fmt.Errorf("openai-responses: text delta for unopened block %d", ev.Index)
		}
		b.text += ev.Text
		return [][]byte{e.frame(streamEvent{Type: "response.output_text.delta", OutputIndex: ev.Index, Delta: ev.Text})}, nil
	case ir.EvThinkingDelta:
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, fmt.Errorf("openai-responses: thinking delta for unopened block %d", ev.Index)
		}
		b.text += ev.Text
		return [][]byte{e.frame(streamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: ev.Index, SummaryIndex: 0, Delta: ev.Text})}, nil
	case ir.EvSigDelta:
		// 签名不进增量事件，随 output_item.done 的 encrypted_content 下发
		if b := e.blocks[ev.Index]; b != nil {
			b.sig += ev.Text
		}
		return nil, nil
	case ir.EvToolInput:
		if e.skip[ev.Index] {
			return nil, nil // 服务端工具块的查询参数无 Responses 形态
		}
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, fmt.Errorf("openai-responses: tool input for unopened block %d", ev.Index)
		}
		b.text += ev.Text
		return [][]byte{e.frame(streamEvent{Type: "response.function_call_arguments.delta", OutputIndex: ev.Index, Delta: ev.Text})}, nil
	case ir.EvBlockStop:
		return e.blockStop(ev.Index), nil
	case ir.EvMessageDelta:
		e.stopReason = ev.StopReason
		e.usage = ev.Usage
		return [][]byte{e.completedFrame()}, nil
	case ir.EvMessageStop:
		return nil, nil // response.completed 已是终止事件
	case ir.EvPing:
		return nil, nil
	case ir.EvError:
		return [][]byte{New().RenderStreamError(ev.Err)}, nil
	}
	return nil, fmt.Errorf("openai-responses: encode unknown event %q", ev.Type)
}

func (e *streamEncoder) blockStart(ev ir.Event) ([][]byte, error) {
	b := &encBlock{typ: blockTypeOf(ev.Block)}
	switch b.typ {
	case ir.BlockThinking:
		b.itemID = e.nextID("rs")
		e.register(ev.Index, b)
		return [][]byte{e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: ev.Index, Item: &inputItem{
			Type: "reasoning", ID: b.itemID, Summary: json.RawMessage(`[]`),
		}})}, nil
	case ir.BlockToolUse:
		b.itemID = e.nextID("fc")
		if ev.Block.ToolUse != nil {
			b.toolID = ev.Block.ToolUse.ID
			b.toolName = ev.Block.ToolUse.Name
		}
		e.register(ev.Index, b)
		return [][]byte{e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: ev.Index, Item: &inputItem{
			Type: "function_call", ID: b.itemID, CallID: b.toolID, Name: b.toolName, Arguments: "",
		}})}, nil
	case ir.BlockServerToolUse, ir.BlockWebSearchToolResult:
		e.skip[ev.Index] = true
		return nil, nil
	default: // text
		b.typ = ir.BlockText
		b.itemID = e.nextID("msg")
		e.register(ev.Index, b)
		added := e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: ev.Index, Item: &inputItem{
			Type: "message", ID: b.itemID, Role: "assistant", Content: json.RawMessage(`[]`),
		}})
		part := e.frame(streamEvent{Type: "response.content_part.added", OutputIndex: ev.Index, ContentIndex: 0, Part: &contentPart{
			Type: "output_text", Text: "",
		}})
		return [][]byte{added, part}, nil
	}
}

func (e *streamEncoder) register(idx int, b *encBlock) {
	e.blocks[idx] = b
	e.order = append(e.order, idx)
}

func (e *streamEncoder) blockStop(idx int) [][]byte {
	b := e.blocks[idx]
	if b == nil || b.closed {
		return nil
	}
	b.closed = true
	return [][]byte{e.frame(streamEvent{Type: "response.output_item.done", OutputIndex: idx, Item: e.doneItem(b)})}
}

// doneItem 由累积状态构造完整 item。
func (e *streamEncoder) doneItem(b *encBlock) *inputItem {
	switch b.typ {
	case ir.BlockThinking:
		it := &inputItem{Type: "reasoning", ID: b.itemID, EncryptedContent: b.sig}
		it.Summary = marshal([]summaryPart{{Type: "summary_text", Text: b.text}})
		return it
	case ir.BlockToolUse:
		args := b.text
		if args == "" {
			args = "{}"
		}
		return &inputItem{Type: "function_call", ID: b.itemID, CallID: b.toolID, Name: b.toolName, Arguments: args}
	default:
		return &inputItem{Type: "message", ID: b.itemID, Role: "assistant",
			Content: marshal([]contentPart{{Type: "output_text", Text: b.text}})}
	}
}

func (e *streamEncoder) completedFrame() []byte {
	e.completed = true
	status := "completed"
	if e.stopReason == ir.StopMaxTokens {
		status = "incomplete"
	}
	return e.frame(streamEvent{Type: "response.completed", Response: &responseObj{
		ID: e.id, Object: "response", CreatedAt: e.created, Model: e.model,
		Status: status, Output: e.fullOutput(), Usage: encodeUsage(e.usage),
	}})
}

// fullOutput 按序输出所有块的完整 item（SDK get_final_response 依赖）。
func (e *streamEncoder) fullOutput() []inputItem {
	idxs := append([]int(nil), e.order...)
	sort.Ints(idxs)
	out := make([]inputItem, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, *e.doneItem(e.blocks[i]))
	}
	return out
}

// Finish 断流兜底：关闭未闭块并补 response.completed。
func (e *streamEncoder) Finish() [][]byte {
	var out [][]byte
	for _, idx := range e.order {
		out = append(out, e.blockStop(idx)...)
	}
	if !e.completed {
		out = append(out, e.completedFrame())
	}
	return out
}

func (e *streamEncoder) frame(ev streamEvent) []byte {
	return []byte("data: " + string(marshal(ev)) + "\n\n")
}

func blockTypeOf(b *ir.Block) ir.BlockType {
	if b == nil {
		return ir.BlockText
	}
	return b.Type
}

// encodeUsage IR -> Responses（input 含 cached 口径）。
func encodeUsage(u *ir.Usage) *usage {
	if u == nil {
		return nil
	}
	out := &usage{
		InputTokens:  u.TotalInput(),
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalInput() + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out.InputTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens,omitempty"`
		}{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		out.OutputTokensDetails = &struct {
			ReasoningTokens int `json:"reasoning_tokens,omitempty"`
		}{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// ---- 非流式响应 ----

func (codec) DecodeResponse(body []byte) (*ir.Response, error) {
	var r responseObj
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai-responses: decode response: %w", err)
	}
	out := &ir.Response{ID: r.ID, Model: r.Model}
	// 复用请求解码的 item 逻辑：把 output items 当成一条对话的尾部
	fake := &ir.Request{}
	for _, it := range r.Output {
		decodeItem(fake, it)
	}
	for _, m := range fake.Messages {
		out.Content = append(out.Content, m.Content...)
	}
	out.StopReason = ir.StopEndTurn
	if r.Status == "incomplete" {
		out.StopReason = ir.StopMaxTokens
	} else {
		for _, b := range out.Content {
			if b.Type == ir.BlockToolUse {
				out.StopReason = ir.StopToolUse
			}
		}
	}
	if r.Usage != nil {
		out.Usage = decodeUsage(r.Usage)
	}
	return out, nil
}

func (codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	fake := &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: resp.Content}}}
	var items []inputItem
	for _, m := range fake.Messages {
		items = append(items, encodeMessageItems(m)...)
	}
	status := "completed"
	if resp.StopReason == ir.StopMaxTokens {
		status = "incomplete"
	}
	return json.Marshal(responseObj{
		ID: resp.ID, Object: "response", CreatedAt: time.Now().Unix(), Model: resp.Model,
		Status: status, Output: items, Usage: encodeUsage(&resp.Usage),
	})
}
