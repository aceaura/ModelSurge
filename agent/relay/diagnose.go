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
	media := map[ir.MediaKind]int{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case ir.BlockToolUse:
				// 非法/非对象参数与协议无关：任何出站都会发生改写（对象槽位
				// 挪进 RawArgsKey，字符串槽位原样透出但工具侧多半也解析不了），
				// 与能力位无关，一律报告。
				if b.ToolUse != nil {
					if _, ok := ir.NormalizeToolInput(b.ToolUse.Input); !ok {
						badArgs++
					}
				}
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
				images++
				if b.Image != nil && b.Image.Data == "" && b.Image.URL != "" {
					urlImages++
				}
			case ir.BlockMedia:
				countMedia(b.Media, media)
			case ir.BlockRefusal:
				refusals++
			case ir.BlockToolResult:
				if b.ToolResult == nil {
					continue
				}
				if b.ToolResult.IsError {
					errResults++
				}
				// tool 结果内嵌的附件与顶层同样会被降级，漏掉这层会让
				// 「工具返回了 PDF」这类丢失完全不可见。
				for _, c := range b.ToolResult.Content {
					if c.Type == ir.BlockMedia {
						countMedia(c.Media, media)
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
	} else if urlImages > 0 && !caps.ImageURLs {
		// 只有形态装不下：上游收 base64 不收远程 URL（kiro）。与整协议无图片能力
		// 分开报，是因为读者的下一步动作不同——这里换成 base64 内联即可。
		notes = append(notes, fmt.Sprintf("dropped %d image(s): upstream accepts inline base64 only, not remote URLs", urlImages))
	}
	notes = append(notes, mediaNotes(media, caps)...)
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
	for _, t := range req.Tools {
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
	return notes
}
