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
	toolKind map[int]ir.ToolKind
	// text 各块已下发的正文。跨协议投影来的引用没有 Raw，编成
	// web_search_result_location 时 cited_text 只能在正文上按范围反推，
	// 而引用总在正文之后到达，所以必须逐块累积。
	text map[int]string
	// droppedSigs 被门控掉的外族/合成签名数，Notes() 收尾时报出。
	droppedSigs int
	// droppedOpaque 被门控掉的外族来源不透明块数。本族块原样随 content_block_start
	// 全量下发，不计数；外族块整块跳过，跳过就要报——静默丢掉正是这条注记机制
	// 要消灭的东西。
	droppedOpaque int
	// droppedTier 没能下发的档位回显原值：越集、或到得太晚（message_delta
	// 没有 service_tier 槽位，chat 系上游的晚到回显送不出去）。
	droppedTier  string
	droppedAudio bool
	badToolArgs  int
	customTools  int
	// droppedCites 先于正文块到达而被丢弃的引用条数。块内偏移相对累积正文
	// 计算，块没开时引用无处可贴，只能丢——但要报出来。
	droppedCites int
	// droppedCitesUnresolved cited_text 反推失败或缺 Required 字段
	// （encrypted_index/url）被丢弃的投影引用条数。丢弃避免整轮 400，
	// 但必须报出来。
	droppedCitesUnresolved int
	tierSent               bool
	messageDeltaSent       bool
	stopped                bool
	// usage 逐事件累计的响应用量：Notes() 按本族槽位算出被丢的细分维度
	// （音频/预测 token 等 anthropic 无对应字段的项）。
	usage ir.Usage
	// sawError 已下发错误帧。错误帧就是终止帧，Finish() 不得再补
	// message_delta+message_stop，否则限流会被告诉客户端「你输出超长了」，
	// 紧接的 message_stop 又把失败伪装成正常结束。
	sawError bool
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{open: map[int]ir.BlockType{}, toolArgs: map[int][]byte{}, toolKind: map[int]ir.ToolKind{}, text: map[int]string{}}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvMessageStart:
		em := encodeMessageStart(ev)
		if ev.Usage != nil {
			e.usage.MergeNonZero(*ev.Usage)
		}
		if ev.ServiceTier != "" {
			if tier, ok := proto.MapServiceTierEcho(ev.ServiceTier, Name); ok {
				em.Usage.ServiceTier = tier
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
		return [][]byte{sseFrame("message_start", marshal(struct {
			Type    string            `json:"type"`
			Message *messageStartBody `json:"message"`
		}{Type: "message_start", Message: em}))}, nil
	case ir.EvBlockStart:
		if ev.Block != nil && !encodableBlock(*ev.Block) {
			// 外族来源的不透明块：不开块，只计数。逐字下发就是一个客户端不认识
			// 的块型，而它没有增量形态，不开块 ⇒ 同下标的 EvBlockStop 找不到已
			// 打开的块，不会留下一个空壳 content_block。损耗经 Notes() 报出。
			e.droppedOpaque++
			return nil, nil
		}
		e.open[ev.Index] = blockTypeOf(ev.Block)
		if e.open[ev.Index] == ir.BlockToolUse {
			e.toolArgs[ev.Index] = nil
			if ev.Block != nil && ev.Block.ToolUse != nil {
				e.toolKind[ev.Index] = ev.Block.ToolUse.Kind
				if ev.Block.ToolUse.Kind == ir.ToolCustom {
					e.customTools++
				}
			}
		}
		return [][]byte{e.blockStartFrame(ev.Index, ev.Block)}, nil
	case ir.EvTextDelta:
		frames := e.ensureOpen(ev.Index, ir.BlockText)
		e.text[ev.Index] += ev.Text
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "text_delta", Text: ev.Text})), nil
	case ir.EvCitation:
		// 不调 ensureOpen：引用不是正文，凭它开一个新块会在客户端多出一个空
		// 文本块，而引用本身要贴的那段正文根本不在里面。块还没开时丢弃——但
		// 必须计数报出（R95 判据：跳过分支无人报等于给静默丢失背书）。
		if _, ok := e.open[ev.Index]; !ok {
			e.droppedCites += len(ev.Citations)
			return nil, nil
		}
		var frames [][]byte
		raws, unresolved := encodeCitations(e.text[ev.Index], ev.Citations)
		e.droppedCitesUnresolved += unresolved
		for _, raw := range raws {
			frames = append(frames, e.deltaFrame(ev.Index, delta{Type: "citations_delta", Citation: raw}))
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
		if e.toolKind[ev.Index] == ir.ToolCustom {
			return frames, nil
		}
		return append(frames, e.deltaFrame(ev.Index, delta{Type: "input_json_delta", PartialJSON: ev.Text})), nil
	case ir.EvBlockStop:
		frames := e.finishToolArgs(ev.Index)
		if !e.closeBlock(ev.Index) {
			return frames, nil
		}
		return append(frames, sseFrame("content_block_stop", marshal(streamEvent{Type: "content_block_stop", Index: ev.Index}))), nil
	case ir.EvMessageDelta:
		// 错误帧已是终止帧：relay 自造错误的序列里 dec.Finish() 还会交来
		// aborted 的 message_delta，发出去就把限流/断流伪装成正常收尾。
		if e.sawError {
			return nil, nil
		}
		e.messageDeltaSent = true
		// message_delta 没有 service_tier 槽位：晚到的回显（chat 系上游的
		// 后续 chunk 才带）即便值集装得下也送不出去，照实报出。
		if ev.ServiceTier != "" && !e.tierSent && e.droppedTier == "" {
			e.droppedTier = ev.ServiceTier
		}
		if ev.Usage != nil {
			e.usage.MergeNonZero(*ev.Usage)
		}
		return [][]byte{sseFrame("message_delta", marshal(streamEvent{
			Type:  "message_delta",
			Delta: &delta{StopReason: UnmapStopReason(ev.StopReason), StopSequence: ev.StopSequence, Container: encodeContainerInfo(ev.Container), StopDetails: encodeStopDetails(ev.StopDetails)},
			Usage: encodeDeltaUsagePtr(ev.Usage),
			// 服务端上下文清理回执挂在事件顶层（与 delta 平级），同族原文
			// 回写；外族投影不进这个槽位（解码侧只有 anthropic 会填）。
			ContextManagement: ev.ContextMgmt,
		}))}, nil
	case ir.EvMessageStop:
		if e.sawError {
			return nil, nil
		}
		e.stopped = true
		return [][]byte{sseFrame("message_stop", marshal(streamEvent{Type: "message_stop"}))}, nil
	case ir.EvPing:
		return [][]byte{sseFrame("ping", marshal(streamEvent{Type: "ping"}))}, nil
	case ir.EvError:
		e.sawError = true
		return [][]byte{New().RenderStreamError(ev.Err)}, nil
	}
	return nil, fmt.Errorf("anthropic: encode unknown event %q", ev.Type)
}

// Finish 冲刷：关闭所有打开的 block，补 message_delta + message_stop。
// 已下发错误帧时只关块——不给客户端留永不结束的 content block，但不再补收尾事件。
func (e *streamEncoder) Finish() [][]byte {
	var out [][]byte
	idxs := make([]int, 0, len(e.open))
	for i := range e.open {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		out = append(out, e.finishToolArgs(i)...)
		out = append(out, sseFrame("content_block_stop", marshal(streamEvent{Type: "content_block_stop", Index: i})))
		delete(e.open, i)
	}
	if e.sawError {
		return out
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
	if e.droppedOpaque > 0 {
		notes = append(notes, proto.OpaqueDropNote(e.droppedOpaque))
		e.droppedOpaque = 0
	}
	if e.droppedTier != "" {
		notes = append(notes, proto.TierEchoDropNote(e.droppedTier))
		e.droppedTier = ""
	}
	if dims := proto.UsageDropDims(&e.usage, Name); len(dims) > 0 {
		notes = append(notes, proto.UsageDetailDropNote(dims))
		e.usage = ir.Usage{}
	}
	if e.droppedAudio {
		notes = append(notes, proto.AudioOutputDropNote())
		e.droppedAudio = false
	}
	if e.badToolArgs > 0 {
		notes = append(notes, ir.RawArgsPassNote(e.badToolArgs))
		e.badToolArgs = 0
	}
	if e.customTools > 0 {
		notes = append(notes, proto.CustomToolDowngradeNote(e.customTools))
		e.customTools = 0
	}
	if e.droppedCites > 0 {
		notes = append(notes, fmt.Sprintf(
			"dropped %d citation(s) that arrived before their text block opened: the receiving side cannot see those sources", e.droppedCites))
		e.droppedCites = 0
	}
	if e.droppedCitesUnresolved > 0 {
		notes = append(notes, fmt.Sprintf(
			"dropped %d citation(s) that lack the required encrypted_index or a resolvable cited_text: the receiving side cannot see those sources", e.droppedCitesUnresolved))
		e.droppedCitesUnresolved = 0
	}
	return notes
}

func (e *streamEncoder) finishToolArgs(index int) [][]byte {
	raw, ok := e.toolArgs[index]
	if !ok {
		return nil
	}
	delete(e.toolArgs, index)
	kind := e.toolKind[index]
	delete(e.toolKind, index)
	if kind == ir.ToolCustom {
		input := (&ir.ToolUse{Kind: kind, InputText: string(raw)}).ObjectInput()
		return [][]byte{e.deltaFrame(index, delta{Type: "input_json_delta", PartialJSON: string(input)})}
	}
	if _, valid := ir.NormalizeToolInput(raw); !valid {
		e.badToolArgs++
	}
	if len(raw) == 0 {
		// 零增量工具块：客户端（含官方 SDK）只从 input_json_delta 拼参数，
		// 一个 delta 都不发就等于参数是空串——拼出来不是合法 JSON。关块前
		// 补一个 "{}" delta（sub2api 同款：clients assemble tool input
		// exclusively from deltas）。
		return [][]byte{e.deltaFrame(index, delta{Type: "input_json_delta", PartialJSON: "{}"})}
	}
	return nil
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
		// 清字段只作用于逐字段序列化。不透明块由 block.MarshalJSON 整块原样吐出，
		// 这里清掉的字段根本不参与它的输出，故无需为它单开分支。
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
	return sseFrame("content_block_start", marshal(streamEvent{Type: "content_block_start", Index: index, ContentBlock: marshal(cb)}))
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

// messageStartBody message_start 的 message 载荷。官方 Message 的 type/role/
// content/usage 都是必填、usage.input_tokens/output_tokens 是必填整数：严格
// SDK 缺键即 ValidationError。chat/gemini 源的 usage 尾到，首帧只能先填 0，
// 由随后的 message_delta 权威修正（new-api 同款处置）。stop_reason/
// stop_sequence 官方首帧为 null，原样照发。
type messageStartBody struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   *string           `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        messageStartUsage `json:"usage"`
	Container    *container        `json:"container,omitempty"`
}

// messageStartUsage 首帧 usage：两个总量必填不省，明细有值才带。
type messageStartUsage struct {
	InputTokens              int                 `json:"input_tokens"`
	OutputTokens             int                 `json:"output_tokens"`
	CacheReadInputTokens     int                 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int                 `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            *cacheCreationUsage `json:"cache_creation,omitempty"`
	ServerToolUse            *serverToolUsage    `json:"server_tool_use,omitempty"`
	InferenceGeo             string              `json:"inference_geo,omitempty"`
	// ServiceTier 档位回显（官方 usage.service_tier）：长在 usage 里，
	// 顶层写这个键是伪造。
	ServiceTier string `json:"service_tier,omitempty"`
}

// encodeMessageStart 构造 message_start 的完整 message 信封。Content 必须是
// 空数组而非 null（官方首帧无内容）。
func encodeMessageStart(ev ir.Event) *messageStartBody {
	mb := &messageStartBody{
		ID:      ev.MessageID,
		Type:    "message",
		Role:    "assistant",
		Model:   ev.Model,
		Content: []json.RawMessage{},
	}
	if u := encodeUsagePtr(ev.Usage); u != nil {
		mb.Usage = messageStartUsage{
			InputTokens:              u.InputTokens,
			OutputTokens:             u.OutputTokens,
			CacheReadInputTokens:     u.CacheReadInputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens,
			CacheCreation:            u.CacheCreation,
			ServerToolUse:            u.ServerToolUse,
			InferenceGeo:             u.InferenceGeo,
		}
	}
	return mb
}

func encodeUsagePtr(u *ir.Usage) *usage {
	if u == nil {
		return nil
	}
	out := &usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheCreationTokens,
	}
	if u.CacheCreationDetailsKnown {
		out.CacheCreation = &cacheCreationUsage{
			Ephemeral5mInputTokens: u.CacheCreation5mTokens,
			Ephemeral1hInputTokens: u.CacheCreation1hTokens,
		}
	}
	if u.WebSearchRequests > 0 || u.WebFetchRequests > 0 {
		out.ServerToolUse = &serverToolUsage{
			WebSearchRequests: u.WebSearchRequests,
			WebFetchRequests:  u.WebFetchRequests,
		}
	}
	if u.ReasoningTokens > 0 {
		out.OutputTokensDetails = &outputTokensDetails{ThinkingTokens: u.ReasoningTokens}
	}
	out.InferenceGeo = u.InferenceGeo
	out.Speed = u.Speed
	return out
}

// encodeDeltaUsagePtr 编 message_delta 的专用 usage。官方 MessageDeltaUsage
// 没有 cache_creation 对象 / inference_geo / service_tier，这里不编——
// 编了就是往帧里写官方 schema 没有的键（同族非流式→流式转换必然触发：
// 聚合 usage 带着明细整体落进 EvMessageDelta）。明细与地理回显由
// message_start 与聚合响应承担，delta 帧不丢可送达的信息。
func encodeDeltaUsagePtr(u *ir.Usage) *messageDeltaUsage {
	if u == nil {
		return nil
	}
	out := &messageDeltaUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheCreationTokens,
	}
	if u.WebSearchRequests > 0 || u.WebFetchRequests > 0 {
		out.ServerToolUse = &serverToolUsage{
			WebSearchRequests: u.WebSearchRequests,
			WebFetchRequests:  u.WebFetchRequests,
		}
	}
	if u.ReasoningTokens > 0 {
		out.OutputTokensDetails = &outputTokensDetails{ThinkingTokens: u.ReasoningTokens}
	}
	return out
}

// encodeStopDetails 拒绝分类回写。空字段省略：上游显式 null 与缺省语义相同，
// IR 不保留二者之别。
func encodeStopDetails(sd *ir.StopDetails) *stopDetails {
	if sd == nil {
		return nil
	}
	return &stopDetails{
		Type:        "refusal",
		Category:    rawStringOrNil(sd.Category),
		Explanation: rawStringOrNil(sd.Explanation),
	}
}

func rawStringOrNil(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	b, _ := json.Marshal(s)
	return b
}

// ---- 非流式响应 ----

func (codec) DecodeResponse(body []byte) (*ir.Response, error) {
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	return &ir.Response{
		ID:                   r.ID,
		Model:                r.Model,
		Content:              decodeBlocks(r.Content),
		StopReason:           MapStopReason(r.StopReason),
		StopSequence:         r.StopSequence,
		StopDetails:          decodeStopDetails(r.StopDetails),
		Usage:                convUsage(r.Usage),
		ServiceTier:          r.Usage.ServiceTier,
		Container:            decodeContainer(r.Container),
		AnthropicContextMgmt: r.ContextManagement,
		AnthropicDiagnostics: r.Diagnostics,
	}, nil
}

func (codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	out := response{
		ID:           resp.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        resp.Model,
		Content:      marshal(encodeBlocks(resp.Content)),
		StopReason:   UnmapStopReason(resp.StopReason),
		StopSequence: resp.StopSequence,
		StopDetails:  encodeStopDetails(resp.StopDetails),
		Usage:        *encodeUsagePtr(&resp.Usage),
	}
	// 值集装不下的回显（OpenAI 的 flex/fast 等）丢弃，由 ResponseNotes 报出。
	// 槽位在 usage 里（官方 usage.service_tier），顶层没有该键。
	if tier, ok := proto.MapServiceTierEcho(resp.ServiceTier, Name); ok {
		out.Usage.ServiceTier = tier
	}
	out.Container = encodeContainerInfo(resp.Container)
	out.ContextManagement = resp.AnthropicContextMgmt
	out.Diagnostics = resp.AnthropicDiagnostics
	return json.Marshal(out)
}

// ResponseNotes 非流式编码损耗扫描：外族签名丢弃 + 对象槽位的畸形参数挪键。
// 本族是附件的原生形态（image / document 块），模型产出的附件不丢。
func (codec) ResponseNotes(resp *ir.Response) []string {
	notes := proto.ScanResponseLosses(resp, Name, false, true, false)
	// cited_text 反推失败或缺 Required 字段（encrypted_index/url）的投影引用在
	// encodeCitations 里整条丢弃（缺键发出整轮必 400）。非流式不走流式编码器的
	// 计数器，这里按同一条判据扫出来。
	unresolved := 0
	for _, b := range resp.Content {
		if b.Type != ir.BlockText {
			continue
		}
		for _, c := range b.Citations {
			if len(c.Raw) == 0 && (c.EncryptedIndex == "" || c.URL == "" || ir.ResolveCitedText(b.Text, c) == "") {
				unresolved++
			}
		}
	}
	if unresolved > 0 {
		notes = append(notes, fmt.Sprintf(
			"dropped %d citation(s) that lack the required encrypted_index or a resolvable cited_text: the receiving side cannot see those sources", unresolved))
	}
	return notes
}
