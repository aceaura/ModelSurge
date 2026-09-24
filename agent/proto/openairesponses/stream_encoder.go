package openairesponses

import (
	"encoding/json"
	"fmt"
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
	// wire IR 块序号 -> wire output_index。IR 序号是解码器的内部编号，可能稀疏
	// 也可能沿用别族的编号习惯；直接写进 output_index 会让客户端按它索引
	// response.output[] 时越界。这里按开块顺序重新稠密编号。
	// 只有真正 register 进 blocks/order 的块才占序号：被 skip 的块（服务端工具、
	// container_upload、涂抹思考、不透明块）不占，否则它们烧掉的序号会让后续块
	// 的 output_index 越过 response.completed 全量 output 的末尾。
	wire    map[int]int
	nextOut int
	// skip 服务端工具块（server_tool_use / web_search_tool_result）的 index。
	// Responses 协议里没有对应 item 类型：它们是上游自己执行的搜索，客户端既
	// 不需要回传也无法回传。落进 text 分支会把查询 JSON 拼进 output_text，
	// 正文里凭空多出一段参数串——宁可不出现。
	// 注意：web_search 一家的调用/结果块不走这里——本族有 web_search_call
	// item 形态，blockStart 里直接映射，wsOpen 负责调用与结果的跨块配对。
	skip map[int]bool
	// wsOpen 待配对结果的 web_search 调用：toolUseID -> IR 块序号。结果块
	// （web_search_tool_result）与调用块在 IR 里是两个独立块，sources 要等
	// 结果块到达才能填进 web_search_call 的 action。
	wsOpen map[string]int

	stopReason          ir.StopReason
	usage               ir.Usage
	hasUsage            bool
	droppedCacheDetails bool
	// tier 已映射待回显的档位（response.created 与终止帧都携带）。
	tier        string
	droppedTier string
	// droppedContainer 容器回显（anthropic 专属）被丢标记：Responses 无该槽位。
	droppedContainer bool
	// droppedUploads 被跳过的 container_upload 块数，Notes() 收尾时报出。
	droppedUploads int
	// droppedRedacted 被跳过的 redacted_thinking 块数，同上。
	droppedRedacted int
	// droppedOpaque 被跳过的不透明块数（源协议专属、IR 里没有块型的未知载荷，
	// 如 web_fetch / code_execution 结果），同上。注意 server_tool_use 与
	// web_search_tool_result 是**有块型**的，不走这里，单独计数。
	droppedOpaque int
	// droppedServerCalls / droppedServerResults 被跳过的托管工具块数：本族
	// 编码器没有为 Anthropic 的 server_tool_use / web_search_tool_result 输出
	// 任何对应 item，同上。
	droppedServerCalls   int
	droppedServerResults int
	// droppedCites 带不出本族的引用条数（Anthropic 的文档类引用没有 URL，
	// 而本族的标注槽位以 URL 为来源身份），同上。
	droppedCites int
	// droppedAudio 完整 Chat 音频输出没有 Responses 流式 item 形态。
	droppedAudio bool
	// droppedImages / droppedFiles 被跳过的模型产出附件块数（image / media）。
	// 本族编码器给助手回合输出的 part 只有 output_text / refusal，没有附件形态。
	// 不显式拦住会落进 default(text) 分支，凭空多出一个 content 为
	// [{"type":"output_text"}] 的空 message item：客户端读到一条没有内容的助手
	// 消息，它还占掉一个 output_index，把后续真块的序号一起推后。
	droppedImages int
	droppedFiles  int
	// droppedSigs 被门控的外族/合成签名数，Notes() 收尾时报出。
	droppedSigs int
	badToolArgs int
	completed   bool
}

type encBlock struct {
	typ              ir.BlockType
	itemID           string
	toolID, toolName string
	toolKind         ir.ToolKind
	text             string // text / thinking / tool input 累积
	sig              string
	cites            []ir.Citation
	closed           bool
	// wsSeeded 开块时 server_tool_use 自带的完整 input（非流式回放路径）。
	// 流式路径 input 经 EvToolInput 增量进 text；最终取 text，空则退回 wsSeeded。
	wsSeeded string
	// wsSources 结果块配对成功后带回的 sources（web_search_call 专用）。
	wsSources []ir.WebSearchResult
	// rawItem 同族不透明 item 的完整线体：added/done 与全量 output 都整块带回。
	rawItem json.RawMessage
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{created: time.Now().Unix(), blocks: map[int]*encBlock{}, wire: map[int]int{}, skip: map[int]bool{}, wsOpen: map[string]int{}}
}

// wireOf IR 块序号 -> 稠密 wire output_index，首次见到时分配。
func (e *streamEncoder) wireOf(i int) int {
	if w, ok := e.wire[i]; ok {
		return w
	}
	w := e.nextOut
	e.nextOut++
	e.wire[i] = w
	return w
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
		// 上游给过创建时间就原值逐帧回写（覆盖构造时的本地钟）；没给才用本地钟。
		if ev.Created != 0 {
			e.created = ev.Created
		}
		e.mapTier(ev.ServiceTier)
		if ev.Container != nil {
			e.droppedContainer = true
		}
		if ev.Audio != nil {
			e.droppedAudio = true
		}
		e.mergeUsage(ev.Usage)
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
			return [][]byte{e.frame(streamEvent{Type: "response.refusal.delta", OutputIndex: idx(e.wireOf(ev.Index)), ContentIndex: idx(0), Delta: ev.Text})}, nil
		}
		return [][]byte{e.frame(streamEvent{Type: "response.output_text.delta", OutputIndex: idx(e.wireOf(ev.Index)), ContentIndex: idx(0), Delta: ev.Text})}, nil
	case ir.EvCitation:
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, nil
		}
		e.droppedCites += proto.CountNonPortableCitations(ev.Citations)
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
				Type: "response.output_text.annotation.added", OutputIndex: idx(e.wireOf(ev.Index)),
				ContentIndex: idx(0), Annotation: &a,
			}))
		}
		return frames, nil
	case ir.EvThinkingDelta:
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, fmt.Errorf("openai-responses: thinking delta for unopened block %d", ev.Index)
		}
		b.text += ev.Text
		return [][]byte{e.frame(streamEvent{Type: "response.reasoning_summary_text.delta", OutputIndex: idx(e.wireOf(ev.Index)), SummaryIndex: idx(0), Delta: ev.Text})}, nil
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
			return nil, nil // 未映射的服务端工具块的查询参数无 Responses 形态
		}
		b := e.blocks[ev.Index]
		if b == nil {
			return nil, fmt.Errorf("openai-responses: tool input for unopened block %d", ev.Index)
		}
		b.text += ev.Text
		if b.typ == ir.BlockServerToolUse {
			// web_search_call 没有参数增量事件：查询串只随 done 帧的 action 下发
			return nil, nil
		}
		typ := "response.function_call_arguments.delta"
		if b.toolKind == ir.ToolCustom {
			typ = "response.custom_tool_call_input.delta"
		}
		return [][]byte{e.frame(streamEvent{Type: typ, OutputIndex: idx(e.wireOf(ev.Index)), Delta: ev.Text})}, nil
	case ir.EvBlockStop:
		return e.blockStop(ev.Index), nil
	case ir.EvMessageDelta:
		// 错误帧已是终止帧（EvError 置 completed）：再发 response.completed
		// 就是把失败伪装成正常结束。
		if e.completed {
			return nil, nil
		}
		e.stopReason = ev.StopReason
		e.mergeUsage(ev.Usage)
		// 晚到的档位回显还补得上：终止帧的 response 对象也带 service_tier。
		e.mapTier(ev.ServiceTier)
		if ev.Container != nil {
			e.droppedContainer = true
		}
		return [][]byte{e.completedFrame()}, nil
	case ir.EvMessageStop:
		return nil, nil // response.completed 已是终止事件
	case ir.EvPing:
		// Responses 没有 ping 事件类型，但 SSE 注释行是协议合法的保活帧：
		// 长思考间隔下吞掉 ping 等于让客户端侧读超时裸奔。
		return [][]byte{[]byte(": ping\n\n")}, nil
	case ir.EvError:
		// 错误帧就是终止帧，置 completed 免得 Finish() 再凭空补一个终止事件：
		// 那时 stopReason 还是零值，补出来的是 response.incomplete 带
		// reason=max_output_tokens——上游过载会被告诉客户端「你输出超长了，
		// 请加大 max_output_tokens」，客户端照做然后再次失败。
		e.completed = true
		return [][]byte{New().RenderStreamError(ev.Err)}, nil
	}
	return nil, fmt.Errorf("openai-responses: encode unknown event %q", ev.Type)
}

func (e *streamEncoder) blockStart(ev ir.Event) ([][]byte, error) {
	b := &encBlock{typ: blockTypeOf(ev.Block)}
	// oi 一律在 register 之后才取：wireOf 是首次见到即分配，先分配再决定跳过
	// 会烧掉一个 output_index，而 response.completed 的全量 output 里并没有
	// 对应条目——客户端拿 output_index 去索引那个数组必然越界。
	switch b.typ {
	case ir.BlockThinking:
		b.itemID = e.nextID("rs")
		e.register(ev.Index, b)
		oi := idx(e.wireOf(ev.Index))
		return [][]byte{e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: oi, Item: &inputItem{
			Type: "reasoning", ID: b.itemID, Summary: json.RawMessage(`[]`),
		}})}, nil
	case ir.BlockToolUse:
		prefix := "fc"
		typ := "function_call"
		if ev.Block.ToolUse != nil {
			b.toolID = ev.Block.ToolUse.ID
			b.toolName = ev.Block.ToolUse.Name
			b.toolKind = ev.Block.ToolUse.Kind
		}
		if b.toolKind == ir.ToolCustom {
			prefix = "ctc"
			typ = "custom_tool_call"
		}
		b.itemID = e.nextID(prefix)
		e.register(ev.Index, b)
		oi := idx(e.wireOf(ev.Index))
		return [][]byte{e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: oi, Item: &inputItem{
			Type: typ, ID: b.itemID, CallID: b.toolID, Name: b.toolName,
		}})}, nil
	case ir.BlockRefusal:
		// 拒绝有独立的 part 类型与独立的 delta 事件名；走 output_text 那条
		// 会让客户端把拒绝当普通回答渲染。
		b.itemID = e.nextID("msg")
		e.register(ev.Index, b)
		oi := idx(e.wireOf(ev.Index))
		added := e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: oi, Item: &inputItem{
			Type: "message", ID: b.itemID, Role: "assistant", Content: json.RawMessage(`[]`),
		}})
		part := e.frame(streamEvent{Type: "response.content_part.added", OutputIndex: oi, ContentIndex: idx(0), Part: &contentPart{
			Type: "refusal",
		}})
		return [][]byte{added, part}, nil
	case ir.BlockServerToolUse:
		// web_search 一家映射成本族的 web_search_call item：sources 等结果块
		// 配对（wsOpen），done 帧在配对成功或 Finish 时发。其它托管调用
		// （web_fetch / code_execution）没有本族输出形态，必须显式拦住，
		// 否则会落进 default(text) 分支凭空多出一个空 output_text 条目。
		stu := ev.Block.ServerToolUse
		if stu == nil || stu.Name != "web_search" || stu.ID == "" {
			e.skip[ev.Index] = true
			e.droppedServerCalls++
			return nil, nil
		}
		b.toolID = stu.ID
		b.toolName = stu.Name
		b.itemID = stu.ID // item id 沿用调用 id：同族往返时 ws_ 原号带回
		b.wsSeeded = string(stu.Input)
		e.register(ev.Index, b)
		e.wsOpen[stu.ID] = ev.Index
		return [][]byte{e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: idx(e.wireOf(ev.Index)), Item: &inputItem{
			Type: "web_search_call", ID: b.itemID, Status: "in_progress",
		}})}, nil
	case ir.BlockWebSearchToolResult:
		// 结果块本身没有 item 形态：sources 填进已开着的 web_search_call
		// 并补发它的 done 帧。找不到待配对的调用（孤儿结果）整块跳过但计数。
		if r := ev.Block.WebSearchToolResult; r != nil {
			if wi, ok := e.wsOpen[r.ToolUseID]; ok {
				delete(e.wsOpen, r.ToolUseID)
				if wb := e.blocks[wi]; wb != nil && !wb.closed {
					wb.wsSources = r.Results
					return [][]byte{e.wsDoneFrame(wi)}, nil
				}
			}
		}
		e.skip[ev.Index] = true
		e.droppedServerResults++
		return nil, nil
	case ir.BlockContainerUpload:
		// 容器文件引用无 Responses 形态：整块跳过但计数，Notes() 报出。
		e.skip[ev.Index] = true
		e.droppedUploads++
		return nil, nil
	case ir.BlockRedactedThinking:
		// 涂抹思考块的不透明密文没有 Responses 形态。不显式拦住会落进下面的
		// default(text) 分支，给客户端凭空多出一个空 output_text 条目；塞进
		// reasoning.encrypted_content 则是伪造 OpenAI 的密文槽位。
		e.skip[ev.Index] = true
		e.droppedRedacted++
		return nil, nil
	case ir.BlockOpaque:
		// 同族的 item 级不透明块（Item 标记，未知托管 item，线体是完整
		// item）：added/done 与全量 output 都按原文整块带回，一字不改。判别按
		// 捕获位置而不是名字——官方 item 型并不都以 _call 结尾
		// （computer_call_output、mcp_list_tools 等）。
		// 其余（源协议专属的 part 级/块级载荷）没有 Responses 形态，必须显式
		// 拦住，否则会落进 default(text) 分支凭空多出一个空 output_text 条目。
		if proto.OpaqueVerbatimFor(ev.Block.Opaque, Name) && ev.Block.Opaque.Item {
			b.rawItem = ev.Block.Opaque.Body
			b.itemID = e.nextID("item")
			e.register(ev.Index, b)
			done := streamEvent{OutputIndex: idx(e.wireOf(ev.Index)), ItemRaw: b.rawItem}
			added := done
			added.Type = "response.output_item.added"
			done.Type = "response.output_item.done"
			b.closed = true // 内容全量随开块下发，blockStop 不再补帧
			return [][]byte{e.frame(added), e.frame(done)}, nil
		}
		e.skip[ev.Index] = true
		e.droppedOpaque++
		return nil, nil
	case ir.BlockImage:
		// 模型产出的图片没有本族输出形态：整块跳过但计数，Notes() 报出。
		e.skip[ev.Index] = true
		e.droppedImages++
		return nil, nil
	case ir.BlockMedia:
		// 文档/音频/视频附件同理：跳过但计数。
		e.skip[ev.Index] = true
		e.droppedFiles++
		return nil, nil
	default: // text
		b.typ = ir.BlockText
		b.itemID = e.nextID("msg")
		e.register(ev.Index, b)
		oi := idx(e.wireOf(ev.Index))
		added := e.frame(streamEvent{Type: "response.output_item.added", OutputIndex: oi, Item: &inputItem{
			Type: "message", ID: b.itemID, Role: "assistant", Content: json.RawMessage(`[]`),
		}})
		part := e.frame(streamEvent{Type: "response.content_part.added", OutputIndex: oi, ContentIndex: idx(0), Part: &contentPart{
			Type: "output_text", Text: "",
		}})
		return [][]byte{added, part}, nil
	}
}

func (e *streamEncoder) register(i int, b *encBlock) {
	if _, dup := e.blocks[i]; !dup {
		e.order = append(e.order, i)
	}
	e.blocks[i] = b
	e.wireOf(i)
}

func (e *streamEncoder) blockStop(i int) [][]byte {
	b := e.blocks[i]
	if b == nil || b.closed {
		return nil
	}
	if b.typ == ir.BlockServerToolUse {
		// web_search_call 的 done 帧等结果块配对（sources 在结果块上）；
		// 结果块始终不来的由 Finish 兜底补发。
		return nil
	}
	b.closed = true
	oi := idx(e.wireOf(i))
	if b.typ == ir.BlockToolUse {
		done := streamEvent{OutputIndex: oi}
		if b.toolKind == ir.ToolCustom {
			done.Type = "response.custom_tool_call_input.done"
			done.Input = b.text
		} else {
			if _, ok := ir.NormalizeToolInput([]byte(b.text)); !ok {
				e.badToolArgs++
			}
			done.Type = "response.function_call_arguments.done"
			// 零增量工具块的累积串是空：空串不是合法 JSON，与同帧
			// output_item.done 里 doneItem 归一出的 "{}" 自相矛盾（客户端按
			// done 帧取参数会拿到两种值）。与 doneItem 同口径归一。
			done.Arguments = b.text
			if done.Arguments == "" {
				done.Arguments = "{}"
			}
		}
		return [][]byte{
			e.frame(done),
			e.frame(streamEvent{Type: "response.output_item.done", OutputIndex: oi, Item: e.doneItem(b)}),
		}
	}
	// part 级终止帧，官方顺序是 *.done -> content_part.done -> output_item.done。
	// 只发 output_item.done 的话，按 part 事件关块的下游永远等不到块结束
	// （cc-switch 把 output_text.done 直接映射成 content_block_stop）。
	var out [][]byte
	switch b.typ {
	case ir.BlockThinking:
		out = append(out,
			e.frame(streamEvent{Type: "response.reasoning_summary_text.done", OutputIndex: oi,
				SummaryIndex: idx(0), Text: b.text}),
			e.frame(streamEvent{Type: "response.reasoning_summary_part.done", OutputIndex: oi,
				SummaryIndex: idx(0), Part: &contentPart{Type: "summary_text", Text: b.text}}),
		)
	case ir.BlockRefusal:
		out = append(out,
			e.frame(streamEvent{Type: "response.refusal.done", OutputIndex: oi, ContentIndex: idx(0),
				Refusal: b.text}),
			e.frame(streamEvent{Type: "response.content_part.done", OutputIndex: oi, ContentIndex: idx(0),
				Part: &contentPart{Type: "refusal", Refusal: b.text}}),
		)
	default:
		as := encodeAnnotations(b.text, b.cites)
		out = append(out,
			e.frame(streamEvent{Type: "response.output_text.done", OutputIndex: oi, ContentIndex: idx(0),
				Text: b.text, Annotations: as}),
			e.frame(streamEvent{Type: "response.content_part.done", OutputIndex: oi, ContentIndex: idx(0),
				Part: &contentPart{Type: "output_text", Text: b.text, Annotations: as}}),
		)
	}
	return append(out, e.frame(streamEvent{Type: "response.output_item.done", OutputIndex: oi, Item: e.doneItem(b)}))
}

// wsDoneFrame web_search_call 的 output_item.done：配对到结果块或 Finish
// 兜底时调用。幂等靠 b.closed。
func (e *streamEncoder) wsDoneFrame(i int) []byte {
	b := e.blocks[i]
	b.closed = true
	return e.frame(streamEvent{Type: "response.output_item.done", OutputIndex: idx(e.wireOf(i)), Item: e.doneItem(b)})
}

// doneItem 由累积状态构造完整 item。
func (e *streamEncoder) doneItem(b *encBlock) *inputItem {
	switch b.typ {
	case ir.BlockServerToolUse:
		input := b.text
		if input == "" {
			input = b.wsSeeded
		}
		return &inputItem{Type: "web_search_call", ID: b.itemID, Status: "completed",
			Action: wsActionFromInput(input, b.wsSources)}
	case ir.BlockThinking:
		it := &inputItem{Type: "reasoning", ID: b.itemID, EncryptedContent: b.sig}
		it.Summary = marshal([]summaryPart{{Type: "summary_text", Text: b.text}})
		return it
	case ir.BlockToolUse:
		if b.toolKind == ir.ToolCustom {
			return &inputItem{Type: "custom_tool_call", ID: b.itemID, CallID: b.toolID, Name: b.toolName, Input: b.text}
		}
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
	var usageOut *usage
	if e.hasUsage {
		usageOut = encodeUsage(&e.usage)
	}
	obj := &responseObj{
		ID: e.id, Object: "response", CreatedAt: e.created, Model: e.model,
		Status: "completed", Output: e.fullOutput(), Usage: usageOut,
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
// 顺序必须是 wire output_index 的顺序（即开块顺序），不能按 IR 序号排：
// 客户端是拿 output_index 去索引这个数组的，两者错位就等于指向别的 item。
func (e *streamEncoder) fullOutput() []json.RawMessage {
	out := make([]json.RawMessage, 0, len(e.order))
	for _, i := range e.order {
		b := e.blocks[i]
		if len(b.rawItem) > 0 {
			out = append(out, b.rawItem) // 同族不透明 item：整块原文带回
			continue
		}
		out = append(out, marshal(e.doneItem(b)))
	}
	return out
}

// Finish 断流兜底：关闭未闭块并补 response.completed。
func (e *streamEncoder) Finish() [][]byte {
	var out [][]byte
	for _, idx := range e.order {
		out = append(out, e.blockStop(idx)...)
		// 结果块始终没到的 web_search_call：补发 done（action 里没有 sources），
		// 不留一个永远 in_progress 的 item 给客户端。
		if b := e.blocks[idx]; b != nil && b.typ == ir.BlockServerToolUse && !b.closed {
			out = append(out, e.wsDoneFrame(idx))
		}
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
	if e.droppedRedacted > 0 {
		notes = append(notes, proto.RedactedThinkingDropNote(e.droppedRedacted))
		e.droppedRedacted = 0
	}
	if e.droppedOpaque > 0 {
		notes = append(notes, proto.OpaqueDropNote(e.droppedOpaque))
		e.droppedOpaque = 0
	}
	if e.droppedServerCalls > 0 || e.droppedServerResults > 0 {
		notes = append(notes, proto.ServerToolDropNote(e.droppedServerCalls, e.droppedServerResults))
		e.droppedServerCalls, e.droppedServerResults = 0, 0
	}
	if e.droppedCites > 0 {
		notes = append(notes, proto.CitationDropNote(e.droppedCites))
		e.droppedCites = 0
	}
	if e.droppedAudio {
		notes = append(notes, proto.AudioOutputDropNote())
		e.droppedAudio = false
	}
	if e.droppedImages > 0 || e.droppedFiles > 0 {
		notes = append(notes, proto.MediaOutputDropNote(e.droppedImages, e.droppedFiles))
		e.droppedImages, e.droppedFiles = 0, 0
	}
	if e.badToolArgs > 0 {
		notes = append(notes, ir.RawArgsPassNote(e.badToolArgs))
		e.badToolArgs = 0
	}
	if e.droppedCacheDetails {
		notes = append(notes, proto.CacheCreationDetailsDropNote())
		e.droppedCacheDetails = false
	}
	return notes
}

func (e *streamEncoder) mergeUsage(u *ir.Usage) {
	if u == nil {
		return
	}
	e.usage.MergeNonZero(*u)
	e.hasUsage = true
	if u.CacheCreationDetailsKnown {
		e.droppedCacheDetails = true
	}
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
	// 官方 Responses API 每帧都带 event: 行（anthropic 侧 sseFrame 早就有）：
	// 按 SSE 事件名分发的客户端对只有 data: 的流一个事件都认不出。
	return []byte("event: " + ev.Type + "\ndata: " + string(marshal(ev)) + "\n\n")
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
	// 终态失败的响应不得伪造成 completed：此前只判 incomplete，failed/cancelled
	// 走到这里会产出 200+空 output 的"成功"，error.code/message 全丢——而流式
	// 路径同事件是报错的，两条路径口径相反（OpenAI SDK status 六值为权威形状）。
	switch r.Status {
	case "failed", "cancelled":
		return nil, errorFromBody(r.Error, "upstream response "+r.Status)
	case "queued", "in_progress":
		// background 模式的非终态：换一个真流式的目标可能成，判可重试。
		return nil, &ir.Error{Type: ir.ErrTypeConnection, Message: "upstream returned non-terminal status " + r.Status, Retryable: true}
	}
	out := &ir.Response{ID: r.ID, Model: r.Model, ServiceTier: r.ServiceTier, Created: r.CreatedAt}
	// 复用请求解码的 item 逻辑：把 output items 当成一条对话的尾部
	fake := &ir.Request{}
	for _, raw := range r.Output {
		var it inputItem
		// raw 已是合法 JSON，这里只可能报字段类型不匹配；Unmarshal 会跳过
		// 冲突字段继续解完其余键。continue 等于让单字段形状冲突销毁整条
		// item（连 default 的不透明块兜底都进不去），改成降级解出——冲突
		// 字段缺省，其余键与原文（opaque 捕获用）都还在（R92a 同款证据）。
		_ = json.Unmarshal(raw, &it)
		decodeItem(fake, it, raw)
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
	wsResults := collectWSResults(fake.Messages)
	for _, m := range fake.Messages {
		items = append(items, encodeMessageItems(m, false, wsResults)...)
	}
	rawItems := make([]json.RawMessage, 0, len(items))
	for i := range items {
		rawItems = append(rawItems, marshal(items[i]))
	}
	// 上游给过创建时间就原值回写；没给才回退本地钟（同 chat 侧口径）。
	created := resp.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	out := responseObj{
		ID: resp.ID, Object: "response", CreatedAt: created, Model: resp.Model,
		Status: "completed", Output: rawItems, Usage: encodeUsage(&resp.Usage),
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

// ResponseNotes 非流式编码损耗扫描：外族签名丢弃；arguments 字符串槽位无损；
// 助手回合的 output items 里没有附件形态，模型产出的图片与文档整块消失。
func (codec) ResponseNotes(resp *ir.Response) []string {
	return proto.ScanResponseLosses(resp, Name, false, false, true)
}
