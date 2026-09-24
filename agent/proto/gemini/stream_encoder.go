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
	// groundingSupports 的 partIndex 指稳定的语义 part，不是网络 chunk 序号。
	// 同一正文/思考/签名块的多个增量只占一个 part；否则引用会随分片漂移。
	texts        map[int]*encText
	thinkingPart map[int]bool
	sigPart      map[int]bool
	partCount    int
	// droppedSigs 被门控的外族/合成签名数；rewrappedArgs 畸形工具参数挪键数。
	// 两者由 Notes() 收尾时报出。
	droppedSigs   int
	rewrappedArgs int
	customTools   int
	// droppedTier 没送出去的档位回显原值：Gemini 响应没有该槽位，恒丢。
	droppedTier string
	// droppedContainer 容器回显（anthropic 专属维度）被丢标记：Gemini 响应
	// 没有 container 槽位。
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
	// 任何 part 形态，同上。
	droppedServerCalls   int
	droppedServerResults int
	// droppedCites 带不出本族的引用条数（Anthropic 的文档类引用没有 URL，
	// 而本族的标注槽位以 URL 为来源身份），同上。
	droppedCites int
	// droppedAudio 完整 Chat 音频输出没有 Gemini 流式响应槽位。
	droppedAudio bool
	// droppedImages / droppedFiles 模型产出的附件里投不出去的部分：既没有 base64
	// 本体也没有 URL（只有本仓不认识的 file_id 引用），inlineData / fileData 两个
	// 槽位都装不下。附件本体投得出去时不计数——它当场就编成 part 下发了。
	droppedImages       int
	droppedFiles        int
	usage               ir.Usage
	hasUsage            bool
	droppedCacheDetails bool
	finished            bool
}

// encText 一个正文块的流式下发记录。
type encText struct {
	text     string
	part     int
	assigned bool
}

type encTool struct {
	name string
	id   string
	kind ir.ToolKind
	args []byte
}

func (codec) NewStreamEncoder() proto.StreamEncoder {
	return &streamEncoder{
		pendingTool:  map[int]*encTool{},
		texts:        map[int]*encText{},
		thinkingPart: map[int]bool{},
		sigPart:      map[int]bool{},
	}
}

func (e *streamEncoder) Encode(ev ir.Event) ([][]byte, error) {
	switch ev.Type {
	case ir.EvMessageStart:
		e.model = ev.Model
		e.id = ev.MessageID
		if ev.ServiceTier != "" {
			e.droppedTier = ev.ServiceTier
		}
		if ev.Container != nil {
			e.droppedContainer = true
		}
		if ev.Audio != nil {
			e.droppedAudio = true
		}
		e.mergeUsage(ev.Usage)
		return nil, nil
	case ir.EvTextDelta:
		t := e.texts[ev.Index]
		if t == nil {
			t = &encText{}
			e.texts[ev.Index] = t
		}
		if !t.assigned {
			t.part = e.partCount
			t.assigned = true
			e.partCount++
		}
		t.text += ev.Text
		return e.chunk([]part{{Text: ev.Text}}, ""), nil
	case ir.EvCitation:
		// 单独一个 chunk 承载 groundingMetadata（Gemini 原生也是在正文 chunk
		// 之后的独立 chunk 里下发）。parts 留空：重发正文会让客户端看到重复文字。
		e.droppedCites += proto.CountNonPortableCitations(ev.Citations)
		gm := encodeGroundingStreamed(e.texts[ev.Index], ev.Citations)
		if gm == nil {
			return nil, nil
		}
		return [][]byte{sseFrame(marshal(generateResponse{
			Candidates:   []candidate{{Content: &content{Role: "model", Parts: []part{}}, GroundingMetadata: gm}},
			ModelVersion: e.model,
			ResponseID:   e.id,
		}))}, nil
	case ir.EvThinkingDelta:
		if !e.thinkingPart[ev.Index] {
			e.thinkingPart[ev.Index] = true
			e.partCount++
		}
		return e.chunk([]part{{Text: ev.Text, Thought: true}}, ""), nil
	case ir.EvSigDelta:
		// thoughtSignature 作为独立 part 下发（Gemini 原生也是如此：
		// 签名在思考文本结束后的单独 part 中到达）。只下发本族真签名：
		// 外族/合成签名占了这一格，客户端下一轮回传必被 Gemini 拒。
		if ev.SignatureFrom != Name {
			e.droppedSigs++
			return nil, nil
		}
		if !e.sigPart[ev.Index] {
			e.sigPart[ev.Index] = true
			e.partCount++
		}
		return e.chunk([]part{{ThoughtSignature: ev.Text}}, ""), nil
	case ir.EvBlockStart:
		if ev.Block != nil && ev.Block.Type == ir.BlockToolUse && ev.Block.ToolUse != nil {
			e.pendingTool[ev.Index] = &encTool{name: ev.Block.ToolUse.Name, id: ev.Block.ToolUse.ID, kind: ev.Block.ToolUse.Kind}
			if ev.Block.ToolUse.Kind == ir.ToolCustom {
				e.customTools++
			}
		}
		if ev.Block != nil && (ev.Block.Type == ir.BlockImage || ev.Block.Type == ir.BlockMedia) {
			// 附件整块到达（没有增量形态），当场下发一个 chunk。槽位与非流式
			// 编码器完全一致（mediaParts 共用）：此前流式一律静默丢掉，同一份
			// 响应按 stream=true/false 请求会得到不同内容。
			parts := mediaParts(ev.Block)
			if len(parts) == 0 {
				if ev.Block.Type == ir.BlockImage {
					e.droppedImages++
				} else {
					e.droppedFiles++
				}
				return nil, nil
			}
			e.partCount += len(parts) // 附件 part 也占部件序号，否则引用索引漂移
			return e.chunk(parts, ""), nil
		}
		if ev.Block != nil && ev.Block.Type == ir.BlockContainerUpload {
			// 容器文件引用无 Gemini part 形态：跳过但计数，Notes() 报出。
			e.droppedUploads++
		}
		if ev.Block != nil && ev.Block.Type == ir.BlockRedactedThinking {
			// 涂抹思考块的不透明密文无 Gemini part 形态（thought part 承载的是
			// 明文 + thoughtSignature，塞密文进去等于伪造签名）：跳过但计数。
			e.droppedRedacted++
		}
		if ev.Block != nil && ev.Block.Type == ir.BlockOpaque {
			// 源协议专属的服务端工具载荷无 Gemini part 形态：跳过但计数。
			e.droppedOpaque++
		}
		if ev.Block != nil {
			switch ev.Block.Type {
			case ir.BlockServerToolUse:
				// 托管工具调用无 Gemini part 形态：groundingMetadata 承载的是本族
				// 检索的来源，塞 Anthropic 的托管搜索记录等于伪造本族凭据。
				// 跳过但计数，Notes() 报出。
				e.droppedServerCalls++
			case ir.BlockWebSearchToolResult:
				// 搜回来的页面同理：跳过但计数。
				e.droppedServerResults++
			}
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
			args := (&ir.ToolUse{Kind: t.kind, Input: t.args, InputText: string(t.args)}).ObjectInput()
			if t.kind != ir.ToolCustom {
				if _, ok := ir.NormalizeToolInput(t.args); !ok {
					e.rewrappedArgs++
				}
			}
			p := content{Role: "model", Parts: []part{{FunctionCall: &functionCall{Name: t.name, Args: args, ID: t.id}}}}
			ensureThoughtSignature(&p)
			e.partCount++ // functionCall part 也占部件序号
			return e.chunk(p.Parts, ""), nil
		}
		return nil, nil
	case ir.EvMessageDelta:
		e.finished = true
		if ev.ServiceTier != "" && e.droppedTier == "" {
			e.droppedTier = ev.ServiceTier
		}
		if ev.Container != nil {
			e.droppedContainer = true
		}
		e.mergeUsage(ev.Usage)
		if e.hasUsage {
			return e.finishChunk(ev.StopReason, &e.usage), nil
		}
		return e.finishChunk(ev.StopReason, nil), nil
	case ir.EvMessageStop:
		return nil, nil
	case ir.EvPing:
		// Gemini 流式没有 ping 帧类型，SSE 注释行是协议合法的保活帧。
		return [][]byte{[]byte(": ping\n\n")}, nil
	case ir.EvError:
		e.finished = true
		// 错误帧之后不得再吐新内容。Gemini 的 functionCall 是攒到块结束才整块下发，
		// 错误到达时缓冲里的工具调用参数必然被截断（形如 `{"q":`），照旧冲刷会让
		// 客户端在错误之后收到一个会真去执行的畸形调用。块已终止，直接丢。
		for idx := range e.pendingTool {
			delete(e.pendingTool, idx)
		}
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
		args := (&ir.ToolUse{Kind: t.kind, Input: t.args, InputText: string(t.args)}).ObjectInput()
		if t.kind != ir.ToolCustom {
			if _, ok := ir.NormalizeToolInput(t.args); !ok {
				e.rewrappedArgs++
			}
		}
		p := content{Role: "model", Parts: []part{{FunctionCall: &functionCall{Name: t.name, Args: args, ID: t.id}}}}
		ensureThoughtSignature(&p)
		e.partCount++
		out = append(out, e.chunk(p.Parts, "")...)
	}
	if !e.finished {
		e.finished = true
		// 上游没给终止 chunk 就断了：按中断档收尾，不伪造 STOP。
		out = append(out, e.finishChunk(ir.StopAborted, nil)...)
	}
	return out
}

// Notes 排干损耗注记（被门控的签名 + 被挪键的畸形工具参数）。
func (e *streamEncoder) Notes() []string {
	var notes []string
	if e.droppedSigs > 0 {
		notes = append(notes, proto.SigDropNote(e.droppedSigs, false))
		e.droppedSigs = 0
	}
	if e.rewrappedArgs > 0 {
		notes = append(notes, ir.RewrapNote(e.rewrappedArgs))
		e.rewrappedArgs = 0
	}
	if e.customTools > 0 {
		notes = append(notes, proto.CustomToolDowngradeNote(e.customTools))
		e.customTools = 0
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
