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
	id, model   string
	created     int64
	tier        string // 已映射待回显的档位（chunk 逐帧携带）
	droppedTier string
	// droppedContainer 容器回显（anthropic 专属）被丢标记：Chat 无该槽位。
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
	// 任何对应形态，同上。
	droppedServerCalls   int
	droppedServerResults int
	// droppedCites 带不出本族的引用条数（Anthropic 的文档类引用没有 URL，
	// 而本族的标注槽位以 URL 为来源身份），同上。
	droppedCites int
	// droppedAudio 完整音频输出来自非流式响应；Chat chunk 无官方 audio 增量槽位。
	droppedAudio bool
	toolIdx      map[int]int // block index -> dense tool index
	toolArgs     map[int][]byte
	toolKind     map[int]ir.ToolKind
	badToolArgs  int
	customTools  int
	skipIdx      map[int]bool // server_tool_use 等无形态块（input delta 丢弃）
	refusalIdx   map[int]bool // 拒绝块序号：其 text delta 走 delta.refusal
	// text 各块已下发的正文，供 annotations 反推 cited_text 与字符索引。
	text     map[int]string
	nextTool int
	// droppedSigs 丢弃的签名增量数：Chat 没有签名槽位，全丢，Notes() 报出。
	droppedSigs int
	// includeUsage 客户端是否显式要流末那个只带 usage 的帧
	// （stream_options.include_usage）。默认不发：OpenAI 契约里该帧只在客户端
	// opt-in 时出现，它的 choices 是空数组，没要的客户端按 choices[0] 取增量
	// 会越界。上游方向恒注入 include_usage（Agent 记账要用），两者刻意解耦。
	includeUsage        bool
	usage               ir.Usage
	hasUsage            bool
	droppedCacheDetails bool
	stopped             bool
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{created: time.Now().Unix(), toolIdx: map[int]int{}, toolArgs: map[int][]byte{}, toolKind: map[int]ir.ToolKind{},
		skipIdx: map[int]bool{}, refusalIdx: map[int]bool{}, text: map[int]string{}}
}

// SetIncludeUsage 下发客户端的 usage 帧意图。实现 proto.UsageOptIn，由
// proto.NewClientStreamEncoder 在建好编码器后调用。
func (e *streamEncoder) SetIncludeUsage(v bool) { e.includeUsage = v }

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
		e.mergeUsage(ev.Usage)
		return [][]byte{e.chunk(&message{Role: "assistant"}, "")}, nil
	case ir.EvBlockStart:
		if ev.Block != nil && ev.Block.Type == ir.BlockToolUse && ev.Block.ToolUse != nil {
			idx := e.nextTool
			e.nextTool++
			e.toolIdx[ev.Index] = idx
			e.toolArgs[ev.Index] = nil
			e.toolKind[ev.Index] = ev.Block.ToolUse.Kind
			if ev.Block.ToolUse.Kind == ir.ToolCustom {
				e.customTools++
			}
			return [][]byte{e.chunk(&message{ToolCalls: []toolCall{{
				Index:    idx,
				ID:       ev.Block.ToolUse.ID,
				Type:     "function",
				Function: functionCall{Name: ev.Block.ToolUse.Name, Arguments: ""},
			}}}, "")}, nil
		}
		if ev.Block != nil && ev.Block.Type == ir.BlockRefusal {
			// 记下块序号：其后的 text delta 要写进 delta.refusal 而不是
			// delta.content，否则拒绝正文会被客户端当成普通回答渲染。
			e.refusalIdx[ev.Index] = true
			return nil, nil
		}
		if ev.Block != nil && ev.Block.Type != ir.BlockText && ev.Block.Type != ir.BlockThinking {
			e.skipIdx[ev.Index] = true // server_tool_use / web_search_tool_result / container_upload / redacted_thinking / opaque
			switch ev.Block.Type {
			case ir.BlockContainerUpload:
				e.droppedUploads++
			case ir.BlockRedactedThinking:
				e.droppedRedacted++
			case ir.BlockOpaque:
				e.droppedOpaque++
			case ir.BlockServerToolUse:
				e.droppedServerCalls++
			case ir.BlockWebSearchToolResult:
				e.droppedServerResults++
			}
		}
		return nil, nil // text/thinking 块开始无需输出
	case ir.EvTextDelta:
		if e.refusalIdx[ev.Index] {
			return [][]byte{e.chunk(&message{Refusal: ev.Text}, "")}, nil
		}
		e.text[ev.Index] += ev.Text
		return [][]byte{e.chunk(&message{Content: json.RawMessage(marshalString(ev.Text))}, "")}, nil
	case ir.EvCitation:
		e.droppedCites += proto.CountNonPortableCitations(ev.Citations)
		as := encodeAnnotations(e.text[ev.Index], ev.Citations)
		if len(as) == 0 {
			return nil, nil
		}
		return [][]byte{e.chunk(&message{Annotations: as}, "")}, nil
	case ir.EvThinkingDelta:
		return [][]byte{e.chunk(&message{ReasoningContent: ev.Text}, "")}, nil
	case ir.EvSigDelta:
		e.droppedSigs++
		return nil, nil // OpenAI 无签名概念，丢弃
	case ir.EvToolInput:
		if e.skipIdx[ev.Index] {
			return nil, nil // 服务端工具块参数无 OpenAI 形态
		}
		idx, ok := e.toolIdx[ev.Index]
		if !ok {
			return nil, fmt.Errorf("openai-chat: tool input for unopened block %d", ev.Index)
		}
		e.toolArgs[ev.Index] = append(e.toolArgs[ev.Index], ev.Text...)
		if e.toolKind[ev.Index] == ir.ToolCustom {
			return nil, nil
		}
		return [][]byte{e.chunk(&message{ToolCalls: []toolCall{{
			Index:    idx,
			Type:     "function",
			Function: functionCall{Arguments: ev.Text},
		}}}, "")}, nil
	case ir.EvBlockStop:
		return e.finishToolArgs(ev.Index), nil
	case ir.EvMessageDelta:
		// 晚到的档位回显（EvMessageStart 之后才解码出来）在 chat 还补得上：
		// 后续 chunk 都带 service_tier。
		e.mapTier(ev.ServiceTier)
		if ev.Container != nil {
			e.droppedContainer = true
		}
		e.mergeUsage(ev.Usage)
		var frames [][]byte
		frames = append(frames, e.chunk(&message{}, UnmapFinishReason(ev.StopReason)))
		if e.hasUsage && e.includeUsage {
			frames = append(frames, e.usageChunk(&e.usage))
		}
		return frames, nil
	case ir.EvMessageStop:
		e.stopped = true
		return [][]byte{[]byte("data: [DONE]\n\n")}, nil
	case ir.EvPing:
		return nil, nil
	case ir.EvError:
		// 错误帧就是终止帧，且 RenderStreamError 自带 [DONE]。不置 stopped 的话
		// Finish() 会再补一个 finish_reason="length" 的 chunk 加第二个 [DONE]：
		// 限流被报成「输出超长」，流末出现两个 [DONE] 也让严格的 SDK 解析失败。
		e.stopped = true
		return [][]byte{New().RenderStreamError(ev.Err)}, nil
	}
	return nil, fmt.Errorf("openai-chat: encode unknown event %q", ev.Type)
}

// Finish 兜底：若流未正常结束，补 finish chunk + [DONE]。
func (e *streamEncoder) Finish() [][]byte {
	var out [][]byte
	for idx := range e.toolArgs {
		out = append(out, e.finishToolArgs(idx)...)
	}
	if e.stopped {
		return out
	}
	e.stopped = true
	// 上游没走到 message_stop 就断了：按中断档收尾（"stop" 会让客户端把
	// 半截输出当成最终答案而不重试）。
	return append(out,
		e.chunk(&message{}, UnmapFinishReason(ir.StopAborted)),
		[]byte("data: [DONE]\n\n"),
	)
}

// Notes 排干损耗注记（Chat 无签名槽位，签名增量全丢；越集档位回显丢弃）。
func (e *streamEncoder) Notes() []string {
	var notes []string
	if e.droppedSigs > 0 {
		notes = append(notes, proto.SigDropNote(e.droppedSigs, true))
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
	if e.badToolArgs > 0 {
		notes = append(notes, ir.RawArgsPassNote(e.badToolArgs))
		e.badToolArgs = 0
	}
	if e.customTools > 0 {
		notes = append(notes, proto.CustomToolDowngradeNote(e.customTools))
		e.customTools = 0
	}
	// 客户端没 opt-in 时 usage 帧压根没发，也就无所谓「TTL 明细被丢」：注记
	// 描述的是实际没能交付的东西，不是协议的静态能力差。照报会让没要 usage 的
	// 客户端读到一条关于自己从未请求的载荷的损耗说明。
	if e.droppedCacheDetails && e.includeUsage {
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

func (e *streamEncoder) finishToolArgs(index int) [][]byte {
	raw, ok := e.toolArgs[index]
	if !ok {
		return nil
	}
	delete(e.toolArgs, index)
	kind := e.toolKind[index]
	delete(e.toolKind, index)
	if kind == ir.ToolCustom {
		idx := e.toolIdx[index]
		input := (&ir.ToolUse{Kind: kind, InputText: string(raw)}).ObjectInput()
		return [][]byte{e.chunk(&message{ToolCalls: []toolCall{{
			Index: idx, Type: "function", Function: functionCall{Arguments: string(input)},
		}}}, "")}
	}
	if _, valid := ir.NormalizeToolInput(raw); !valid {
		e.badToolArgs++
	}
	return nil
}

// mapTier 映射档位回显：值集装不下的（anthropic 的 batch、responses 的
// ultrafast 等）丢弃，Notes() 报出。重复到达时先到先得，不覆盖不重复报。
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

func (e *streamEncoder) chunk(delta *message, finishReason string) []byte {
	return []byte("data: " + string(marshal(response{
		ID:          e.id,
		Object:      "chat.completion.chunk",
		Created:     e.created,
		Model:       e.model,
		Choices:     []choice{{Index: 0, Delta: delta, FinishReason: finishReason}},
		ServiceTier: e.tier,
	})) + "\n\n")
}

func (e *streamEncoder) usageChunk(u *ir.Usage) []byte {
	return []byte("data: " + string(marshal(response{
		ID:          e.id,
		Object:      "chat.completion.chunk",
		Created:     e.created,
		Model:       e.model,
		Choices:     []choice{},
		Usage:       encodeUsage(u),
		ServiceTier: e.tier,
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

func (c codec) DecodeResponse(body []byte) (*ir.Response, error) {
	resp, _, err := c.DecodeResponseWithNotes(body)
	return resp, err
}

func (codec) DecodeResponseWithNotes(body []byte) (*ir.Response, []string, error) {
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, nil, fmt.Errorf("openai-chat: decode response: %w", err)
	}
	out := &ir.Response{ID: r.ID, Model: r.Model, ServiceTier: r.ServiceTier}
	selected := primaryChoice(r.Choices)
	if selected != nil && selected.Message != nil {
		m := selected.Message
		if len(m.Audio) > 0 && string(m.Audio) != "null" {
			var a audioOutput
			if json.Unmarshal(m.Audio, &a) == nil {
				out.Audio = &ir.AudioOutput{ID: a.ID, Data: a.Data, ExpiresAt: a.ExpiresAt, Transcript: a.Transcript}
			}
		}
		if m.ReasoningContent != "" {
			out.Content = append(out.Content, ir.Block{Type: ir.BlockThinking, Thinking: &ir.Thinking{Text: m.ReasoningContent}})
		}
		out.Content = append(out.Content, attachCitations(contentBlocks(m.Content), decodeAnnotations(m.Annotations))...)
		if m.Refusal != "" {
			out.Content = append(out.Content, ir.Block{Type: ir.BlockRefusal, Text: m.Refusal})
		}
		for _, tc := range m.ToolCalls {
			out.Content = append(out.Content, ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			}})
		}
		out.StopReason = MapFinishReason(selected.FinishReason)
	}
	if r.Usage != nil {
		out.Usage = decodeUsage(r.Usage)
	}
	var notes []string
	if len(r.Choices) > 1 {
		notes = append(notes, proto.AdditionalChoicesDropNote(len(r.Choices)-1))
	}
	return out, notes, nil
}

func primaryChoice(choices []choice) *choice {
	if len(choices) == 0 {
		return nil
	}
	selected := 0
	for i := range choices {
		if choices[i].Index == 0 {
			return &choices[i]
		}
		if choices[i].Index < choices[selected].Index {
			selected = i
		}
	}
	return &choices[selected]
}

func (codec) EncodeResponse(resp *ir.Response) ([]byte, error) {
	msg := &message{Role: "assistant"}
	if resp.Audio != nil {
		msg.Audio = marshal(audioOutput{
			ID: resp.Audio.ID, Data: resp.Audio.Data, ExpiresAt: resp.Audio.ExpiresAt, Transcript: resp.Audio.Transcript,
		})
	}
	var text string
	var cites []ir.Citation
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockText:
			cites = append(cites, shiftCitations(b.Citations, text, b.Text)...)
			text += b.Text
		case ir.BlockRefusal:
			msg.Refusal += b.Text
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
	msg.Annotations = encodeAnnotations(text, cites)
	out := response{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []choice{{Index: 0, Message: msg, FinishReason: UnmapFinishReason(resp.StopReason)}},
		Usage:   encodeUsage(&resp.Usage),
	}
	// 值集装不下的回显（anthropic 的 batch、responses 的 ultrafast）丢弃，
	// 由 ResponseNotes 报出。
	if tier, ok := proto.MapServiceTierEcho(resp.ServiceTier, Name); ok {
		out.ServiceTier = tier
	}
	return json.Marshal(out)
}

// ResponseNotes 非流式编码损耗扫描：Chat 无签名槽位（签名全丢），
// arguments 是字符串槽位（透传无损）。
func (codec) ResponseNotes(resp *ir.Response) []string {
	return proto.ScanResponseLosses(resp, Name, true, false)
}
