package relay

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/proto"
)

// writeLossyNotes 把有损诊断落到日志与响应头。须在 WriteHeader 之前调用。
func writeLossyNotes(w http.ResponseWriter, target string, notes []string) {
	if len(notes) == 0 {
		return
	}
	joined := strings.Join(notes, "; ")
	log.Printf("relay: upstream %s lossy conversion: %s", target, joined)
	w.Header().Set("X-ModelSurge-Notes", joined)
}

// writeRespNotes 响应侧损耗注记：非流式方向在 WriteHeader 前并入
// X-ModelSurge-Notes（与请求侧注记同头，"; " 连接）。
func writeRespNotes(w http.ResponseWriter, client string, notes []string) {
	if len(notes) == 0 {
		return
	}
	joined := strings.Join(notes, "; ")
	log.Printf("relay: client %s response-side loss: %s", client, joined)
	if prev := w.Header().Get("X-ModelSurge-Notes"); prev != "" {
		joined = prev + "; " + joined
	}
	w.Header().Set("X-ModelSurge-Notes", joined)
}

// logRespNotes 流式方向的响应侧注记只能进日志（头已发）；
// 客户端可见部分由 proto.SSENoteFrames 的注释帧承担。
func logRespNotes(client string, notes []string) {
	if len(notes) == 0 {
		return
	}
	log.Printf("relay: client %s response-side loss: %s", client, strings.Join(notes, "; "))
}

// countMedia 按大类累计附件。nil 也计入 MediaOther：块类型已经是 media，
// 载荷却没有内容，这本身就是该丢的东西，静默跳过会漏报。
func countMedia(m *ir.Media, into map[ir.MediaKind]int) {
	if m == nil {
		into[ir.MediaOther]++
		return
	}
	into[m.Kind]++
}

// mediaNotes 逐大类比对上游能力。按大类分别报而不是合成一条，是因为读者的
// 下一步动作不同：音频装不下要先转文字，PDF 装不下要先抽文本，认不出大类的
// 附件则要先确认 MIME。降级成占位文本块的事实一并说明，否则客户端会以为
// 附件原样送达了。
func mediaNotes(counts map[ir.MediaKind]int, caps proto.Capabilities) []string {
	var notes []string
	for _, c := range []struct {
		kind ir.MediaKind
		ok   bool
		what string
	}{
		{ir.MediaDocument, caps.Documents, "document"},
		{ir.MediaAudio, caps.Audio, "audio attachment"},
		{ir.MediaVideo, caps.Video, "video attachment"},
		// other 大类没有对应能力位：MIME 认不出来就无法判断上游能否承载，
		// 一律按文档能力放行（文档槽位是各协议里最宽松的不透明容器）。
		{ir.MediaOther, caps.Documents, "attachment of unrecognized type"},
	} {
		if n := counts[c.kind]; n > 0 && !c.ok {
			notes = append(notes, fmt.Sprintf(
				"replaced %d %s(s) with a placeholder text block: upstream protocol has no slot for it", n, c.what))
		}
	}
	return notes
}

// Diagnose 对比请求特征与上游协议能力，返回本次转换必然发生的有损点描述。
// protoName 为上游协议名（codec.Name），用于判断 thinking 签名能否在该上游回放。
// 目的是让有损转换可观测（日志 + X-ModelSurge-Notes），而不是静默丢信息
// （参考 new-api RequestResult.Diagnostics 的思想）。
func Diagnose(req *ir.Request, protoName string, caps proto.Capabilities) []string {
	var notes []string

	sigs, foreign, images, urlImages, errResults, refusals, badArgs := 0, 0, 0, 0, 0, 0, 0
	uploads, audioRefs, customCalls, customResults, redacted, opaque := 0, 0, 0, 0, 0, 0
	serverCalls, serverResults := 0, 0
	docCtx, docCites := 0, 0
	noPayload, details, fileRefs := 0, 0, 0
	media := map[ir.MediaKind]int{}
	// countImage 归类一张图片。顶层与工具结果内嵌的都走这里：两侧编码器同样会
	// 跳过无载荷的图片、同样会在目标没有档位槽位时丢掉 detail，漏掉内嵌那层会让
	// 「工具返回了一张图」这类丢失完全不可见。
	countImage := func(img *ir.Image) {
		images++
		if img == nil {
			noPayload++
			return
		}
		// 能力判定只在这一处发生：每个计数器都是「相对目标协议确实丢了」的口径，
		// 出注记时只看计数、不再问一次能力位。两处都问会让其中一处变成死代码——
		// 任一侧被改坏行为都不变，损耗也就无从见证。
		if img.Detail != "" && !caps.ImageDetail {
			details++
		}
		// 「没有载荷」同样是相对目标协议而言的：只给 file_id 的图片在 Responses
		// 一族是完整可投递的，投给其余三家才无从表达。四个分支互斥——同一张图报
		// 两次会让读者以为是两张。
		switch {
		case img.HasPayload():
			// 图片本体送达；顺带带了个目标不认的 file_id 不值得单报，本体已经在
			// 了，那个引用只是冗余载体。只有形态装不下（上游收 base64 不收远程
			// URL）才计数——与整协议无图片能力分开报，是因为读者的下一步动作
			// 不同：这里换成 base64 内联即可。
			if img.Data == "" && !caps.ImageURLs {
				urlImages++
			}
		case img.FileID != "" && caps.ImageFileRef:
			// 只凭文件引用即可投递，本目标装得下。
		case img.FileID != "":
			fileRefs++
		default:
			noPayload++
		}
	}
	for _, m := range req.Messages {
		if m.Role == ir.RoleAssistant && m.AudioID != "" {
			audioRefs++
		}
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockToolUse:
				// 非法/非对象参数与协议无关：任何出站都会发生改写（对象槽位
				// 挪进 RawArgsKey，字符串槽位原样透出但工具侧多半也解析不了），
				// 与能力位无关，一律报告。
				if b.ToolUse != nil {
					if b.ToolUse.Kind == ir.ToolCustom {
						customCalls++
					} else if _, ok := ir.NormalizeToolInput(b.ToolUse.Input); !ok {
						badArgs++
					}
				}
			case ir.BlockContainerUpload:
				uploads++
			case ir.BlockRedactedThinking:
				redacted++
			case ir.BlockOpaque:
				// 只有解码出它的那一族能原样带回（判定与 codec 的编码分支同源）。
				// codex 与 openai-responses 同形，靠 WireFamily 归一，不然这条同形
				// 通道会被误判成跨族而报一次根本没发生的损耗。
				if !proto.OpaqueVerbatimFor(b.Opaque, protoName) {
					opaque++
				}
			case ir.BlockServerToolUse:
				serverCalls++
			case ir.BlockWebSearchToolResult:
				serverResults++
			case ir.BlockThinking:
				if b.Thinking != nil && b.Thinking.Signature != "" {
					sigs++
					// 签名的协议形态与上游不一致（含来源不明）：
					// codec 会保守降级，否则上游签名校验会 400
					if b.Thinking.SignatureFrom != protoName {
						foreign++
					}
				}
			case ir.BlockImage:
				countImage(b.Image)
			case ir.BlockMedia:
				countMedia(b.Media, media)
				c, s := proto.DocConfigOf(b.Media)
				docCtx += c
				docCites += s
			case ir.BlockRefusal:
				refusals++
			case ir.BlockToolResult:
				if b.ToolResult == nil {
					continue
				}
				if b.ToolResult.Kind == ir.ToolCustom {
					customResults++
				}
				if b.ToolResult.IsError {
					errResults++
				}
				// tool 结果内嵌的附件与顶层同样会被降级，漏掉这层会让
				// 「工具返回了 PDF」这类丢失完全不可见。
				for _, c := range b.ToolResult.Content {
					if c.Type == ir.BlockMedia {
						countMedia(c.Media, media)
						dc, ds := proto.DocConfigOf(c.Media)
						docCtx += dc
						docCites += ds
					}
					if c.Type == ir.BlockImage {
						countImage(c.Image)
					}
				}
			}
		}
	}
	if sigs > 0 && !caps.ThinkingSignature {
		notes = append(notes, fmt.Sprintf("dropped %d thinking signature(s): upstream protocol cannot replay them", sigs))
	} else if foreign > 0 {
		notes = append(notes, fmt.Sprintf("dropped %d thinking signature(s): not issued by a %s-protocol upstream; replaying them cross-protocol would be rejected", foreign, protoName))
	}
	if images > 0 && !caps.Images {
		notes = append(notes, fmt.Sprintf("dropped %d image(s): upstream protocol has no image input", images))
	} else {
		if urlImages > 0 {
			notes = append(notes, fmt.Sprintf("dropped %d image(s): upstream accepts inline base64 only, not remote URLs", urlImages))
		}
		if noPayload > 0 {
			// 与能力位无关：图片部件没有任何可投递的载荷，照编上去是一个缺必填键
			// 的形状（Anthropic 的 base64 source 缺 media_type/data，OpenAI 两系
			// 写出 url:"" 或整个 image_url 键都没有），三个出站族的上游都会 400
			// 拒整轮。整块跳过后至少报错点落在诊断里而不是上游的拒信上。
			notes = append(notes, fmt.Sprintf(
				"dropped %d image(s): the part carries no payload the target protocol can express (no base64, no URL, no usable file reference); an empty image part would be rejected upstream", noPayload))
		}
		if details > 0 {
			// 档位决定上游怎么切图、进而决定输入 token 计费（low 固定 85 token，
			// high 按原图分块）。丢掉之后上游一律按自己的默认档处理，客户端指定的
			// 成本控制静默失效，账单上看得出、请求里看不出。
			notes = append(notes, fmt.Sprintf(
				"dropped the resolution tier on %d image(s): the target protocol has no detail slot, the upstream will tile them at its own default level instead", details))
		}
		if fileRefs > 0 {
			// 图片字节从未内联进请求体，本层也不代取上游文件服务，所以这一维装不下
			// 就是彻底没了——与 URL 那条「换成 base64 即可」不同，读者无从补救。
			notes = append(notes, fmt.Sprintf(
				"dropped the file reference on %d image(s): the target protocol's image slot cannot point at a file-service id, and the image bytes were never inlined in the request, so they cannot be recovered here", fileRefs))
		}
	}
	notes = append(notes, mediaNotes(media, caps)...)
	if (docCtx > 0 || docCites > 0) && protoName != "anthropic" {
		// 文档块上的用途旁注与引用开关是 anthropic 专属配置：外族的附件槽位
		// 只装文件本身。附件本体照常投递，故与 mediaNotes 的大类降级分开报。
		notes = append(notes, proto.DocumentConfigDropNote(docCtx, docCites))
	}
	if refusals > 0 && !caps.Refusal {
		// 历史里的拒绝会被并进普通文本发给上游。读者能做的是别把它当模型的
		// 正常回答引用——上游看到的已经是不带标记的文本了。
		notes = append(notes, fmt.Sprintf(
			"merged %d refusal(s) into plain text: upstream protocol has no refusal field, the model cannot tell it previously refused", refusals))
	}
	if errResults > 0 && !caps.ToolResultError {
		// 失败的工具结果在上游看来与成功结果同形，模型会把报错文本当成
		// 正常返回值继续推理。读者能做的是把失败信息写进结果文本本身。
		notes = append(notes, fmt.Sprintf("dropped error flag on %d tool result(s): upstream protocol cannot mark a tool call as failed", errResults))
	}
	if badArgs > 0 {
		// 最常见来源是上一轮 max_tokens 把参数 JSON 截断。对象槽位挪进
		// 显式键位（清空会让工具不带参数执行）；字符串槽位原样透出，
		// 工具侧解析会失败——两种后果不同，分开说。
		if caps.ToolInputObject {
			notes = append(notes, fmt.Sprintf(
				"rewrapped %d tool call argument(s): malformed or non-object JSON moved to %s, the tool will not receive its parameters", badArgs, ir.RawArgsKey))
		} else {
			notes = append(notes, fmt.Sprintf(
				"passed through %d malformed tool call argument(s) verbatim: the tool will fail to parse them", badArgs))
		}
	}
	if uploads > 0 && protoName != "anthropic" {
		// 历史里的容器文件引用块无处安放：目标协议没有 container_upload
		// 形态，模型看不到之前送进容器/由容器产出的文件。file_id 属客户端
		// 凭据，不抄进注记。
		notes = append(notes, fmt.Sprintf(
			"dropped %d container upload block(s): the target protocol has no container file-reference slot, the model cannot see files previously uploaded to or produced by the code-execution container", uploads))
	}
	if redacted > 0 && protoName != "anthropic" {
		// 历史里的涂抹思考块无处安放：密文只有 Anthropic 能解，外族既没有
		// 承载槽位也不能据此恢复思考链。密文不进注记。
		notes = append(notes, fmt.Sprintf(
			"dropped %d redacted thinking block(s): the target protocol has no opaque-reasoning slot, the encrypted thinking state cannot be replayed", redacted))
	}
	if opaque > 0 {
		// 历史里的不透明块无处安放：块型只在产出它的那一族里有定义（Anthropic 的
		// web_fetch / code_execution / tool_search 结果、search_result，OpenAI 两系
		// 客户端发来的未知 content part），目标协议没有承载它载荷的槽位。逐字发过去
		// 就是一个目标上游不认识的块型，会被按块型校验直接 400 拒整轮，所以只能整块
		// 跳过。块体属会话内容，不进注记。
		// 有块型的 server_tool_use / web_search_tool_result 不走这里，见下条。
		notes = append(notes, fmt.Sprintf(
			"dropped %d opaque content block(s): the block type is only defined in the protocol that produced it, the target has no slot for its payload, so the model cannot see the content the client put in that block in earlier turns", opaque))
	}
	if (serverCalls > 0 || serverResults > 0) && protoName != "anthropic" {
		// 历史里的托管工具块：两种块型成对出现（调用 + 结果），一起丢反而不会
		// 撕毁 tool_use/tool_result 配平，上游不会拒——但模型看不到自己上一轮
		// 让网关搜了什么、搜回了哪些页面，只能重新搜一遍。搜索结果的标题/URL/
		// 摘要属会话内容，不进注记。
		notes = append(notes, proto.ServerToolDropNote(serverCalls, serverResults))
	}
	if audioRefs > 0 && protoName != "openai-chat" {
		// 音频 id 是 Chat 多轮上下文中的服务端引用，外族既没有引用槽位，
		// 也不能据此取回音频本体；值本身不进入注记。
		notes = append(notes, fmt.Sprintf(
			"dropped %d assistant audio reference(s): the target protocol has no replay-id slot, the model cannot recover audio generated in earlier turns", audioRefs))
	}
	if !caps.Sampling {
		var params []string
		if req.Temperature != nil {
			params = append(params, "temperature")
		}
		if req.TopP != nil {
			params = append(params, "top_p")
		}
		if len(req.StopSequences) > 0 {
			params = append(params, "stop_sequences")
		}
		if req.MaxTokens > 0 {
			params = append(params, "max_tokens")
		}
		if len(params) > 0 {
			notes = append(notes, "dropped sampling parameter(s) "+strings.Join(params, ",")+": upstream payload has no such fields")
		}
	}
	if !caps.Citations {
		if n := ir.CountCitations(req); n > 0 {
			// 历史消息里的来源标注会被抹掉。读者能做的是别指望模型在后续轮次里
			// 复述出处——它看到的正文已经不带任何来源了。
			notes = append(notes, fmt.Sprintf(
				"dropped %d citation(s): upstream protocol has no slot for source annotations", n))
		}
	} else if protoName != "anthropic" {
		// 有槽位，但槽位以 URL 为来源身份：Anthropic 的文档类引用只有
		// document_index 与页/块/字符下标，装不进去。caps.Citations 是个
		// 整族布尔量，看不见这种「逐条装不下」的损耗，必须单独数。
		if n := ir.CountNonPortableCitations(req); n > 0 {
			notes = append(notes, proto.CitationDropNote(n))
		}
	}
	if req.TopK != nil && !caps.TopK {
		notes = append(notes, "dropped top_k: upstream protocol has no equivalent field")
	}
	notes = append(notes, samplingNotes(req, protoName, caps)...)
	if rf := req.ResponseFormat; rf != nil {
		// 这一条比别的更要紧：客户端会直接 JSON.parse 响应，拿到自由文本就是
		// 硬失败而非降级。读者能做的是把 schema 写进 system 提示自行约束。
		switch {
		case !caps.StructuredOutput:
			what := "JSON output mode"
			if rf.IsSchema() {
				what = "JSON schema constraint"
			}
			notes = append(notes, "dropped "+what+": upstream payload has no structured output field, the response will be free-form text")
		case caps.StructuredOutputSchemaOnly && !rf.IsSchema():
			// anthropic 的 output_config.format 只有 json_schema 一种 type：
			// 「只要求合法 JSON」这一档给不出。
			notes = append(notes,
				"dropped JSON output mode: upstream protocol accepts only schema-constrained structured output, the response will be free-form text")
		}
	}
	if req.ToolChoice != nil && req.ToolChoice.DisableParallel && !caps.ParallelToolCalls {
		notes = append(notes, "dropped parallel tool call restriction: upstream protocol cannot express it")
	}
	if req.Thinking != nil && req.Thinking.Enabled && req.ToolChoice != nil &&
		(req.ToolChoice.Mode == ir.ChoiceAny || req.ToolChoice.Mode == ir.ChoiceTool) &&
		!caps.ThinkingForcedToolChoice {
		notes = append(notes, "downgraded tool_choice to auto: upstream rejects forced tool choice while thinking is enabled")
	}

	var dropped, unmapped []string
	customDefs, customFormats := 0, 0
	for _, t := range req.Tools {
		if t.Kind == ir.ToolCustom {
			customDefs++
			if len(t.Format) > 0 {
				customFormats++
			}
		}
		if t.Hosted == "" {
			continue
		}
		switch {
		case !caps.HostedTools:
			dropped = append(dropped, t.Hosted)
		case t.Hosted != ir.HostedWebSearch && t.Hosted != ir.HostedCodeExecution:
			unmapped = append(unmapped, t.Hosted) // 无跨协议映射的种类，即使上游支持托管工具也只能透传同族
		}
	}
	if protoName != "openai-responses" && protoName != "codex" {
		if customDefs > 0 {
			notes = append(notes, fmt.Sprintf(
				"downgraded %d custom tool definition(s) to function declarations: upstream protocol has no free-form tool type, input is exposed as a required string field", customDefs))
		}
		if customFormats > 0 {
			notes = append(notes, fmt.Sprintf(
				"dropped format from %d custom tool definition(s): upstream protocol cannot enforce the text or grammar constraint", customFormats))
		}
		if customCalls > 0 {
			notes = append(notes, proto.CustomToolDowngradeNote(customCalls))
		}
		if customResults > 0 {
			notes = append(notes, fmt.Sprintf(
				"downgraded %d custom tool output(s) to ordinary function results: upstream protocol has no custom_tool_call_output item", customResults))
		}
		if req.ToolChoice != nil && req.ToolChoice.ToolKind == ir.ToolCustom {
			notes = append(notes,
				"downgraded custom tool_choice to a named function choice: upstream protocol has no custom tool selector")
		}
	}
	if len(dropped) > 0 {
		notes = append(notes, "dropped hosted tool(s) "+strings.Join(dropped, ",")+": upstream protocol cannot execute them")
	}
	if len(unmapped) > 0 {
		notes = append(notes, "no cross-protocol mapping for hosted tool(s) "+strings.Join(unmapped, ","))
	}
	return notes
}

// samplingNotes 调参维度装不下时的说明。这一批一律只报不拒：拒绝会把一个能用
// 的回答换成零回答，而上游协议是调度层按策略选的、客户端无从预知，让它为一个
// 自己控制不了的路由结果吃 400，故障归因方向是错的。要强制可用 request_overrides。
//
// 措辞要说清后果而不只是字段名：读者看到 "dropped n" 读不出「按数组取第二个
// 候选会越界」，而那才是它要改的代码。
func samplingNotes(req *ir.Request, protoName string, caps proto.Capabilities) []string {
	var notes []string
	if !caps.Penalties {
		if req.PresencePenalty != nil {
			notes = append(notes, "dropped presence_penalty: upstream protocol has no penalty parameter")
		}
		if req.FrequencyPenalty != nil {
			notes = append(notes, "dropped frequency_penalty: upstream protocol has no penalty parameter")
		}
	}
	if req.Seed != nil && !caps.Seed {
		notes = append(notes, "dropped seed: upstream protocol has no seed parameter, results are not reproducible")
	}
	if req.Candidates != nil && !caps.Candidates {
		notes = append(notes, "dropped n: upstream protocol has no multi-candidate parameter, only one candidate will be returned")
	}
	if !caps.LogProbs && (req.LogProbs != nil || req.TopLogProbs != nil) {
		notes = append(notes, "dropped logprobs: upstream protocol has no log probability parameter")
	}
	if len(req.LogitBias) > 0 && !caps.LogitBias {
		// 不翻译：偏置的键是 token id，词表随模型而变，跨模型重映射没有正确答案。
		notes = append(notes, "dropped logit_bias: upstream protocol has no logit bias parameter")
	}
	if req.Metadata["user_id"] != "" && !caps.UserID {
		notes = append(notes, "dropped user id: upstream protocol has no end-user identifier parameter, abuse tracking will not see it")
	}
	if req.SafetyIdentifier != "" {
		// safety_identifier 与 user 同一维度：出站 anthropic 且 user_id 槽被占
		// 时挤不进去，其余无 UserID 位的协议直接丢。值不回显。
		switch {
		case !caps.UserID:
			notes = append(notes,
				"dropped safety identifier: upstream protocol has no abuse-tracking identifier parameter, the upstream safety system will not see it")
		case caps.OpenAIExtras:
			// OpenAI 两系有原生槽位，不丢不报。
		default:
			// 只剩 anthropic：映进 metadata.user_id，该槽被 user 占了才丢。
			if req.Metadata["user_id"] != "" {
				notes = append(notes,
					"dropped safety identifier: the request already carries a user id, only one identifier reaches the upstream's abuse tracking")
			}
		}
	}
	if len(req.SafetySettings) > 0 {
		// safetySettings 是 Gemini 独有维度，没有任何出站接得住，恒报。
		notes = append(notes, fmt.Sprintf(
			"dropped %d safety setting(s): upstream protocol has no content-safety threshold parameter, filtering falls back to the upstream default", len(req.SafetySettings)))
	}
	if req.CachedContent != "" {
		// cachedContent 同理：缓存是服务端资源 id，换协议后引用不到。
		notes = append(notes, "dropped cached content reference: upstream protocol has no context-caching parameter, the full context will be sent and billed")
	}
	if req.PreviousResponseID != "" && !caps.ResponseChain {
		notes = append(notes, "dropped previous_response_id: upstream protocol has no response-chaining parameter, only the items in this request will reach the model")
	}
	if req.Store != nil && *req.Store && !caps.ResponseChain {
		notes = append(notes, "dropped store=true: upstream protocol has no response-storage switch, the response will not be retrievable later")
	}
	if req.ItemRefs > 0 {
		// item_reference 恒报：代理无状态解析不了引用，展开成 IR 后即便回
		// responses 出站也无法复原——被引用的内容不会到达上游。
		notes = append(notes, fmt.Sprintf(
			"unresolved %d item reference(s): the proxy is stateless and cannot expand them, referenced content will not reach the upstream", req.ItemRefs))
	}
	if req.ConversationID != "" && !caps.ResponseChain {

		// 会话对象锚点是会话链语义的另一种形态（与 previous_response_id
		// 互斥），与链锚点同一位门控；别的协议没有服务端会话概念。
		notes = append(notes,
			"dropped conversation anchor: the target protocol has no server-side conversation, context continues only via the messages in this request")
	}
	if len(req.ContextMgmt) > 0 && !caps.ResponseChain {
		// 服务端压缩策略同为 responses 一族专属：丢了上游按默认策略
		// （不压缩或默认阈值）处理，超长上下文行为与客户端预期不符。
		notes = append(notes,
			"dropped context_management: the target protocol has no server-side context compaction configuration, the upstream default policy applies")
	}
	if req.Background != nil && *req.Background && !caps.ResponsesExtras {
		// 客户端期待的异步行为会变成同步等待；显式 false 等同默认，不算丢。
		notes = append(notes,
			"dropped background mode: the target protocol runs synchronously, the client expecting an async job will get a blocking response")
	}
	if len(req.Include) > 0 && !caps.ResponsesExtras {
		// include 点名的额外回传载荷（logprobs/加密推理/检索结果等）在
		// 其他三族的响应 schema 里没有任何对应物。
		notes = append(notes, fmt.Sprintf(
			"dropped %d include value(s): the client asked for extra payload back, but the target protocol has no such mechanism", len(req.Include)))
	}
	if req.Prompt != nil && !caps.ResponsesExtras {
		// prompt 模板内容存在服务端，代理展开不了；丢了它上游只能看到
		// 裸消息——模板里的指令全部丢失。
		notes = append(notes,
			"dropped prompt template reference: the template content lives server-side and the target protocol cannot resolve it, its instructions will not reach the upstream")
	}
	if req.ServiceTier != "" {
		switch {
		case !caps.ServiceTier:
			notes = append(notes,
				"dropped service tier: the target protocol has no capacity tier field, scheduling falls back to the upstream default")
		default:
			// 有槽位不代表装得下：三家值集不同，provably 无等价的档位
			// （如 anthropic 的 priority、chat 的 ultrafast）照实报出。
			if _, ok := proto.MapServiceTier(req.ServiceTier, protoName); !ok {
				notes = append(notes, fmt.Sprintf(
					"dropped service tier %q: the target protocol's tier set has no equivalent, scheduling falls back to the upstream default", req.ServiceTier))
			}
		}
	}
	if req.PromptCacheKey != "" && !caps.PromptCacheKey {
		// 值是客户端自选串，不回显（与 user id 同款纪律）。
		notes = append(notes,
			"dropped prompt cache key: the target protocol has no cache routing field, repeated prefixes may recompute instead of hitting the cache")
	}
	if req.Verbosity != "" && !caps.OpenAIExtras {
		notes = append(notes,
			"dropped verbosity setting: the target protocol has no output-length steering field, the model decides how verbose to be")
	}
	if len(req.Moderation) > 0 && !caps.OpenAIExtras {
		notes = append(notes,
			"dropped moderation policy: the target protocol has no request-level moderation parameter, moderation falls back to the upstream default")
	}
	if len(req.PromptCacheOptions) > 0 && !caps.OpenAIExtras {
		notes = append(notes,
			"dropped prompt cache options: the target protocol has no explicit cache breakpoint control, caching follows the upstream default policy")
	}
	if protoName != "openai-chat" {
		// modalities/audio/prediction/web_search_options 四维是 chat 一族
		// 专属（responses 全系 SDK 零命中）。audio 依附 modalities：模态
		// 丢了音频配置必然随之丢，合并成一则报；prediction 与
		// web_search_options 各自独立。
		if len(req.Modalities) > 0 || req.AudioOut != nil {
			notes = append(notes,
				"dropped modalities/audio config: the target protocol cannot request audio output, the response will be text-only")
		}
		if len(req.Prediction) > 0 {
			notes = append(notes,
				"dropped prediction config: the target protocol has no predicted-output parameter, the regeneration speedup the client asked for will not happen")
		}
		if len(req.WebSearchOptions) > 0 {
			notes = append(notes,
				"dropped web_search_options: the target protocol has no web-search tuning parameter, search behavior follows the upstream default")
		}
	}
	if protoName != "anthropic" {
		// 缓存断点是 anthropic 专属维度（块级 cache_control + tools[].cache_control）。
		// 跨族丢断点此前完全静默：客户端精心放置的断点蒸发后，缓存命中率与
		// 计费都变，客户端却看不到任何迹象。数清块与工具两处。
		n := 0
		for _, b := range req.System {
			if b.CacheCtl != "" {
				n++
			}
		}
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.CacheCtl != "" {
					n++
				}
			}
		}
		for _, t := range req.Tools {
			if t.CacheCtl != "" {
				n++
			}
		}
		// 顶层 cache_control 便捷糖官方语义=自动一个断点，计入同一维度。
		if req.TopCacheCtl != "" {
			n++
		}
		if n > 0 {
			notes = append(notes, fmt.Sprintf(
				"dropped %d cache breakpoint(s): the target protocol has no prompt-caching breakpoint parameter, cached prefixes may be reprocessed and billed", n))
		}
		// 推理地理偏好同为 anthropic 专属：外族没有任何对应参数，
		// 丢了请求会落到 workspace 默认区域，合规敏感的客户端必须知道。
		if req.InferenceGeo != "" {
			notes = append(notes,
				"dropped inference_geo: the target protocol has no geographic-region preference, inference runs wherever the upstream's default region is")
		}
		// 代码执行容器复用标识与技能声明：外族没有容器概念，丢了上游
		// 只能开新容器、技能不加载，客户端期待的状态全丢。
		if req.Container != nil {
			notes = append(notes,
				"dropped container parameter: the target protocol has no code-execution container reuse or skill declaration, the upstream starts with a fresh container and no skills loaded")
		}
	}
	if !caps.ToolStrict {
		n := 0
		for _, t := range req.Tools {
			if t.Strict != nil {
				n++
			}
		}
		if n > 0 {
			notes = append(notes, fmt.Sprintf(
				"dropped strict flag on %d tool(s): the target protocol has no schema-strictness switch, tool call arguments are not guaranteed to validate against the schema", n))
		}
	}
	if protoName != "anthropic" {
		// anthropic 工具定义的 2026 修饰四维（defer_loading / eager_input_streaming /
		// input_examples / allowed_callers）其余协议一个都没有。数带修饰的工具数。
		n := 0
		for _, t := range req.Tools {
			if t.DeferLoading || t.EagerInputStreaming != nil ||
				len(t.InputExamples) > 0 || len(t.AllowedCallers) > 0 {
				n++
			}
		}
		if n > 0 {
			notes = append(notes, fmt.Sprintf(
				"dropped tool modifiers on %d tool(s): the target protocol has no defer-loading, eager-streaming, input-example or caller-restriction fields, tools behave with the upstream defaults", n))
		}
	}
	if t := req.Thinking; t != nil && protoName != "anthropic" {
		// adaptive（模型自主决定思考量）与 display（思考回显形态）都是
		// anthropic 专属维度：OpenAI 的 effort 是显式档位、reasoning.summary
		// 是啰嗦程度而非可见性，都不构成等价物，不映射只报出。
		if t.Adaptive {
			notes = append(notes,
				"dropped adaptive thinking: the target protocol only takes an explicit effort level, a fixed level will be used instead of the model choosing")
		}
		if t.Display != "" {
			notes = append(notes,
				"dropped thinking display preference: the target protocol has no visibility control for reasoning content, thinking is echoed in the upstream default form")
		}
	}
	if t := req.Thinking; t != nil && protoName != "openai-responses" && protoName != "codex" {
		// reasoning 的摘要详略与 context/mode 只有 responses 一族有槽位：chat 的
		// reasoning_effort 是单值、anthropic 的 thinking 块只管开关与预算，
		// 三维都装不下，丢弃并报出。
		if t.Summary != "" {
			notes = append(notes, fmt.Sprintf(
				"dropped reasoning summary preference %q: the target protocol has no summary-verbosity field, reasoning summaries come in the upstream default form", t.Summary))
		}
		if len(t.Context) > 0 {
			notes = append(notes,
				"dropped reasoning context scope: the target protocol's reasoning parameter takes only an effort level, reasoning runs over the upstream default context")
		}
		if len(t.Mode) > 0 {
			notes = append(notes,
				"dropped reasoning mode: the target protocol's reasoning parameter takes only an effort level, reasoning runs in the upstream default mode")
		}
	}
	if t := req.Thinking; t != nil && protoName == "anthropic" {
		// anthropic 的 effort 值集封闭五值（low/medium/high/xhigh/max）：
		// minimal 与未知值 provably 装不下；"none" 与未开思考同义，静默。
		switch t.Effort {
		case "", "none", "low", "medium", "high", "xhigh", "max":
		case "minimal":
			notes = append(notes,
				"dropped minimal thinking effort: the target protocol's effort set starts at low, the upstream default level applies")
		default:
			notes = append(notes, fmt.Sprintf(
				"dropped thinking effort %q: the target protocol only accepts low, medium, high, xhigh or max, the upstream default level applies", t.Effort))
		}
	}
	return notes
}
