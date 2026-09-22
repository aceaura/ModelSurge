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
	// tier 已映射待回显的档位（response.created 与终止帧都携带）。
	tier        string
	droppedTier string
	// droppedContainer 容器回显（anthropic 专属）被丢标记：Responses 无该槽位。
	droppedContainer bool
	// droppedUploads 被跳过的 container_upload 块数，Notes() 收尾时报出。
	droppedUploads int
	// droppedAudio 完整 Chat 音频输出没有 Responses 流式 item 形态。
	droppedAudio bool
	// droppedSigs 被门控的外族/合成签名数，Notes() 收尾时报出。
	droppedSigs int
	badToolArgs int
	completed   bool
}

type encBlock struct {
	typ              ir.BlockType
	itemID           string
	toolID, toolName string
	text             string // text / thinking / arguments 累积
	sig              string
	cites            []ir.Citation
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
		e.mapTier(ev.ServiceTier)
		if ev.Container != nil {
			e.droppedContainer = true
		}
		if ev.Audio != nil {
			e.droppedAudio = true
		}
		return [][]byte{e.frame(streamEvent{Type: "response.created", Response: &responseObj{
			ID: e.id, Object: "response", CreatedAt: e.created, Model: e.model, Status: "in_progress",
			ServiceTier: e.tier,
		}})}, nil
	case ir.EvBlockStart:
		return e.blockStart(ev)
	case ir.EvTextDelta:
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, fmt.Errorf("openai-responses: text delta for unopened block %d", ev.Index)
		}
		b.text += ev.Text
		if b.typ == ir.BlockRefusal {
			return [][]byte{e.frame(streamEvent{Type: "response.refusal.delta", OutputIndex: ev.Index, Delta: ev.Text})}, nil
		}
		return [][]byte{e.frame(streamEvent{Type: "response.output_text.delta", OutputIndex: ev.Index, Delta: ev.Text})}, nil
	case ir.EvCitation:
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, nil
		}
		as := encodeAnnotations(b.text, ev.Citations)
		if len(as) == 0 {
			return nil, nil
		}
		// 同时累到块上：output_item.done 的 part 要带全量 annotations，
		// 只发增量事件的话读 final response 的 SDK 拿不到任何引用。
		b.cites = append(b.cites, ev.Citations...)
		frames := make([][]byte, 0, len(as))
		for i := range as {
			a := as[i]
			frames = append(frames, e.frame(streamEvent{
				Type: "response.output_text.annotation.added", OutputIndex: ev.Index,
				ContentIndex: 0, Annotation: &a,
			}))
		}
		return frames, nil
	case ir.EvThinkingDelta:
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, fmt.Errorf("openai-responses: thinking delta for unopened block %d", ev.Index)
		}
		b.text += ev.Text
		return [][]byte{e.frame(streamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: ev.Index, SummaryIndex: 0, Delta: ev.Text})}, nil
	case ir.EvSigDelta:
		// 签名不进增量事件，随 output_item.done 的 encrypted_content 下发。
		// 只收本族真签名：外族/合成签名放进 encrypted_content 会被客户端当成
		// 可回传的 reasoning 凭据，下一轮必被上游拒。
		if ev.SignatureFrom != Name {
			e.droppedSigs++
			return nil, nil
		}
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
		// 晚到的档位回显还补得上：终止帧的 response 对象也带 service_tier。
		e.mapTier(ev.ServiceTier)
		if ev.Container != nil {
			e.droppedContainer = true
		}
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
	case ir.BlockRefusal:
		// 拒绝有独立的 part 类型与独立的 delta 事件名；走 output_text 那条
		// 会让客户端把拒绝当普通回答渲染。
		b.itemID = e.nextID("msg")
		e.register(ev.Index, b)
		added := e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: ev.Index, Item: &inputItem{
			Type: "message", ID: b.itemID, Role: "assistant", Content: json.RawMessage(`[]`),
		}})
		part := e.frame(streamEvent{Type: "response.content_part.added", OutputIndex: ev.Index, ContentIndex: 0, Part: &contentPart{
			Type: "refusal",
		}})
		return [][]byte{added, part}, nil
	case ir.BlockServerToolUse, ir.BlockWebSearchToolResult:
		e.skip[ev.Index] = true
		return nil, nil
	case ir.BlockContainerUpload:
		// 容器文件引用无 Responses 形态：整块跳过但计数，Notes() 报出。
		e.skip[ev.Index] = true
		e.droppedUploads++
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
	if b.typ == ir.BlockToolUse {
		if _, ok := ir.NormalizeToolInput([]byte(b.text)); !ok {
			e.badToolArgs++
		}
	}
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
	case ir.BlockRefusal:
		return &inputItem{Type: "message", ID: b.itemID, Role: "assistant",
			Content: marshal([]contentPart{{Type: "refusal", Refusal: b.text}})}
	default:
		return &inputItem{Type: "message", ID: b.itemID, Role: "assistant",
			Content: marshal([]contentPart{{Type: "output_text", Text: b.text,
				Annotations: encodeAnnotations(b.text, b.cites)}})}
	}
}

func (e *streamEncoder) completedFrame() []byte {
	e.completed = true
	obj := &responseObj{
		ID: e.id, Object: "response", CreatedAt: e.created, Model: e.model,
		Status: "completed", Output: e.fullOutput(), Usage: encodeUsage(e.usage),
		ServiceTier: e.tier,
	}
	// 事件名也要跟着改。此前恒发 response.completed 只改 status 字段，而本仓的
	// 解码器（与官方 SDK）是按事件名分支的，completed 分支不看 status——
	// responses -> responses 往返会把截断整个吃掉，读成正常结束。
	typ := "response.completed"
	if reason := unmapIncompleteReason(e.stopReason); reason != "" {
		typ = "response.incomplete"
		obj.Status = "incomplete"
		obj.IncompleteDetails = &incompleteDetails{Reason: reason}
	}
	return e.frame(streamEvent{Type: typ, Response: obj})
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
		// 唯一置 stopReason 的地方是 EvMessageDelta，而它同时发出终止帧置
		// completed——所以走到这里必然没收到终止事件，流是被中断的。留空会让
		// completedFrame 发出 response.completed，客户端读成正常完成。
		e.stopReason = ir.StopAborted
		out = append(out, e.completedFrame())
	}
	return out
}

// Notes 排干损耗注记（被门控的外族/合成签名、越集丢弃的档位回显）。
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
	if e.droppedContainer {
		notes = append(notes, proto.ContainerDropNote())
		e.droppedContainer = false
	}
	if e.droppedUploads > 0 {
		notes = append(notes, proto.ContainerUploadDropNote(e.droppedUploads))
		e.droppedUploads = 0
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

// mapTier 映射档位回显：值集装不下的（anthropic 的 batch 等）丢弃，
// Notes() 报出。重复到达时先到先得，不覆盖不重复报。
func (e *streamEncoder) mapTier(raw string) {
	if raw == "" || e.tier != "" || e.droppedTier != "" {
		return
	}
	if tier, ok := proto.MapServiceTierEcho(raw, Name); ok {
		e.tier = tier
	} else {
		e.droppedTier = raw
	}
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
	out := &ir.Response{ID: r.ID, Model: r.Model, ServiceTier: r.ServiceTier}
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
		out.StopReason = mapIncompleteReason(&r)
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
		items = append(items, encodeMessageItems(m, false)...)
	}
	out := responseObj{
		ID: resp.ID, Object: "response", CreatedAt: time.Now().Unix(), Model: resp.Model,
		Status: "completed", Output: items, Usage: encodeUsage(&resp.Usage),
	}
	// 值集装不下的回显（anthropic 的 batch）丢弃，由 ResponseNotes 报出。
	if tier, ok := proto.MapServiceTierEcho(resp.ServiceTier, Name); ok {
		out.ServiceTier = tier
	}
	// 风控拦截与输出超长在 Responses 里是同一个 status 的两个 reason；
	// 只写 status 会让客户端把拦截当成超长，转而去加大 max_output_tokens。
	if reason := unmapIncompleteReason(resp.StopReason); reason != "" {
		out.Status = "incomplete"
		out.IncompleteDetails = &incompleteDetails{Reason: reason}
	}
	return json.Marshal(out)
}

// ResponseNotes 非流式编码损耗扫描：外族签名丢弃；arguments 字符串槽位无损。
func (codec) ResponseNotes(resp *ir.Response) []string {
	return proto.ScanResponseLosses(resp, Name, false, false)
}
