// Package proto 定义协议 codec 接口与注册表。
// 每个协议（anthropic / openai-chat / openai-responses / gemini）实现一个 Codec，
// 通过 Register 注册；跨协议转换经由 ir 中转，协议包之间互不依赖。
package proto

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// Capabilities 协议能力声明。relay 层在转发前对比请求特征与上游能力，
// 把必然发生的有损转换记入诊断（日志 + X-ModelSurge-Notes），避免静默丢信息。
type Capabilities struct {
	ThinkingSignature        bool // thinking 签名可双向保真（Anthropic signature / Responses encrypted_content / Gemini thoughtSignature）
	Images                   bool // 图片输入
	HostedTools              bool // 服务端托管工具声明（web_search / code_execution 等）
	ThinkingForcedToolChoice bool // 思考模式下允许 tool_choice 强制（required/指定函数）；DeepSeek 系 Chat 上游会 400

	// ImageURLs 远程 URL 形态的图片输入。与 Images 分开是因为两者粒度不同：
	// 「整个协议没有图片能力」与「只是这一种形态装不下」是同形不同因，
	// 合用一位会让诊断把读者指向错误的下一步。当前四个出站两种形态都收，
	// 两位恒真，那条 URL 分支与 Images 分支一样是留给未来出站 codec 的缝。
	ImageURLs bool

	// Sampling 采样参数（temperature / top_p / stop_sequences / max_tokens）
	// 能随请求送达上游。为假时客户端调的参数全部无效。
	Sampling bool

	// TopK 独立于 Sampling：只有 Anthropic 与 Gemini 有这一维，
	// OpenAI 两系原生没有对应字段，不是「能力缺失」而是协议里就不存在。
	TopK bool

	// ParallelToolCalls 可表达「禁止并行工具调用」。Anthropic 是
	// disable_parallel_tool_use，Chat 与 responses 都是 parallel_tool_calls。
	ParallelToolCalls bool

	// StructuredOutput 结构化输出约束（JSON 模式 / JSON Schema）能随请求送达。
	// Chat 是 response_format，Responses 是 text.format，Gemini 是
	// responseMimeType + responseSchema，Anthropic 是 output_config.format
	// （2026 年新增）。装不下时客户端拿到的会是自由文本、JSON.parse 会失败；
	// 把 schema 写进 system 提示或声明单工具后强制调用都在改写请求语义，
	// 本层不做。
	StructuredOutput bool

	// StructuredOutputSchemaOnly 结构化输出只接 schema 约束形态，纯 JSON
	// 模式（只要求合法 JSON、不给 schema）没有槽位。Anthropic 的
	// output_config.format 只有 json_schema 一种 type，是唯一的受限者；
	// 其余三家两种形态都能表达。
	StructuredOutputSchemaOnly bool

	// Documents PDF 等文档附件输入。Anthropic 是 document 块，Chat 是 file 部分，
	// Responses 是 input_file，Gemini 是 inlineData（MIME 白名单含 application/pdf）。
	Documents bool

	// Audio 音频附件输入。只有 Chat 的 input_audio、Responses 的 input_audio 与
	// Gemini 的 inlineData 有；Anthropic 完全没有音频入口。
	// 与 Documents 分开是因为 Anthropic 能收文档但收不了音频，合位会把两者的
	// 诊断结论弄反，而读者的下一步动作不同（转文字 vs 保留原附件）。
	Audio bool

	// Video 视频附件输入。只有 Gemini 有原生槽位。
	Video bool

	// Refusal 有独立的「模型拒绝作答」槽位。OpenAI 两系有（Chat 的
	// message.refusal、Responses 的 refusal content part），Anthropic 与 Gemini
	// 没有——那两家只有 stop_reason/finishReason 能表达「这是拒绝」，
	// 正文只能并入普通文本。装不下时降级为文本而非丢弃：拒绝正文是模型真正
	// 说出的话，丢了客户端只剩一条空消息。
	Refusal bool

	// ToolResultError 工具结果能标出「这次调用失败了」。Anthropic 是
	// is_error，Gemini 靠 response 里的 error 键约定。
	// OpenAI 两系的 tool / function_call_output 里没有任何这类标志：失败结果
	// 与成功结果同形，模型只能从文本自行猜测。
	ToolResultError bool

	// Citations 正文的来源标注有槽位。Anthropic 是 text.citations，Chat 是
	// message.annotations，Responses 是 output_text.annotations，Gemini 是
	// groundingMetadata。装不下时正文照常送达，丢的是「这句话出自哪里」——
	// 客户端会把有出处的结论渲染成模型的自由发挥。
	Citations bool

	// 以下是调参维度的承载能力。为假时照常发请求、只出诊断说明：拒绝会把一个
	// 能用的回答换成零回答，而目标协议是调度层按策略选的、客户端无从预知，
	// 让它为一个自己控制不了的路由结果吃 400，故障归因方向是错的。

	// Penalties presence_penalty / frequency_penalty。Chat 与 Gemini 有，
	// Anthropic 与 Responses 的载荷里没有这一维。
	Penalties bool

	// Seed 确定性种子。只有 Chat 与 Gemini 有。
	Seed bool

	// Candidates 多候选（Chat 的 n、Gemini 的 candidateCount）。
	// 装不下时上游只回一路，客户端按数组取第二个候选会越界。
	Candidates bool

	// LogProbs 对数概率。Chat 有独立开关 + top_logprobs，Responses 只有
	// top_logprobs，Gemini 是 responseLogprobs + logprobs。
	LogProbs bool

	// LogProbsViaTopN 为真表示本协议没有独立的 logprobs 开关，top_logprobs
	// 兼任开关与档位（Responses 是这样）。此时客户端只给 logprobs 会什么都
	// 拿不到，出站须补一个档位——目标满足得了的请求不该因字段形状不同而落空。
	LogProbsViaTopN bool

	// LogitBias token 偏置。只有 Chat 有这一维。
	LogitBias bool

	// ToolInputObject 工具参数槽位是 JSON 对象形态（Anthropic input、
	// Gemini args）而非字符串形态（Chat/Responses arguments）。
	// 对象槽位装不下非法/非对象参数，会被规整进 ir.RawArgsKey 键位；
	// 字符串槽位原样透传。两者诊断措辞不同，读者要改的地方也不同。
	ToolInputObject bool

	// UserID 有终端用户标识槽位（Anthropic metadata.user_id、
	// Chat/Responses 的 user）。
	UserID bool

	// ResponseChain 有服务端会话链槽位（Responses 的 previous_response_id
	// 与 store）。只有 responses 一族（含 codex 别名）有。
	ResponseChain bool

	// ResponsesExtras 有 Responses 一族的专属请求修饰槽位：
	// include（点名要额外回传载荷）、background（后台运行）、
	// prompt（服务端 prompt 模板引用）。只有 responses 一族
	// （含 codex 别名）有；其他三族连「对应物不存在」都谈不上——
	// 这些概念只在 Responses 里有。conversation 锚点归 ResponseChain 位。
	ResponsesExtras bool

	// ServiceTier 有服务质量档位槽位。Anthropic（auto/standard_only）、
	// Chat 与 Responses（auto/default/flex/scale/priority/fast，Responses
	// 另有 ultrafast）三家值集不同：有槽位不代表装得下所有值，跨族
	// 可映射性由 MapServiceTier 判定。
	ServiceTier bool

	// PromptCacheKey 有提示缓存路由键槽位（OpenAI 两系的
	// prompt_cache_key）。Anthropic 走显式 cache_control 断点，
	// 没有路由键概念。
	PromptCacheKey bool

	// OpenAIExtras 有 OpenAI 两系 2026 新增的请求修饰槽位：
	// verbosity（输出啰嗦程度档位）、moderation（请求级审核策略）、
	// prompt_cache_options（显式缓存断点）。Chat 与 Responses 都有
	// （verbosity 在 Responses 挪进了 text 下）；anthropic 一个都没有。
	// safety_identifier 不归此位——它与 user 同维度，归 UserID 位。
	OpenAIExtras bool

	// ToolStrict 工具定义有 strict 槽位（schema 严格校验保证）：
	// anthropic tool.strict、OpenAI 两系 function.strict。
	ToolStrict bool
}

// MapServiceTier 把 service_tier 原值映射到目标协议值集。
// 返回 ok=false 表示目标协议的值集里 provably 没有等价物（出站丢 + 诊断）。
// 映射规则：
//   - auto 三家都有，恒通；
//   - standard_only（anthropic 方言）与 default（OpenAI 方言）互译，
//     语义同为「只用标准容量，不占优先级」；
//   - ultrafast 是 responses 专属（gpt-5.6-sol 审批制），chat 与 anthropic
//     的值集 provably 没有它；
//   - 其余 OpenAI 方言值（flex/scale/priority/fast）去 anthropic 无等价；
//   - 目标族内不认识的值原样透传——可能是我们核对 SDK 之后官方新增的档位，
//     丢了比让上游照实 400 更糟（只报不拒）。
func MapServiceTier(tier, protoName string) (string, bool) {
	if tier == "auto" {
		return "auto", true
	}
	switch protoName {
	case "anthropic":
		switch tier {
		case "standard_only":
			return tier, true
		case "default":
			return "standard_only", true
		}
		return "", false
	case "openai-chat":
		switch tier {
		case "standard_only":
			return "default", true
		case "ultrafast":
			return "", false
		}
		return tier, true
	default: // openai-responses / codex：值集是 chat 的超集
		if tier == "standard_only" {
			return "default", true
		}
		return tier, true
	}
}

// MapServiceTierEcho 把上游回显的实际档位映射到目标协议的回显值集。
// 回显语义是「实际用了哪档服务容量」，值集与请求侧偏好不同：
// anthropic 回显 standard/priority/batch，chat 回显
// auto/default/flex/scale/priority/fast，responses 另有 ultrafast。
// 映射规则：
//   - standard/standard_only（anthropic 方言）与 default（OpenAI 方言）
//     互译，同为标准容量；
//   - priority 三家都有，恒通；
//   - batch 只有 anthropic 回显得出，OpenAI 两系值集 provably 没有；
//   - auto/flex/scale/fast 去 anthropic 无等价；ultrafast 仅 responses 系；
//   - gemini 没有回显槽位，恒 false。
//
// 返回 ok=false 表示越集（出站丢 + 注记，值是枚举非敏感，可带值报出）。
func MapServiceTierEcho(tier, protoName string) (string, bool) {
	switch tier {
	case "standard", "standard_only":
		tier = "default"
	}
	switch protoName {
	case "anthropic":
		switch tier {
		case "default":
			return "standard", true
		case "priority", "batch":
			return tier, true
		}
		return "", false
	case "openai-chat":
		switch tier {
		case "auto", "default", "flex", "scale", "priority", "fast":
			return tier, true
		}
		return "", false
	case "openai-responses", "codex":
		switch tier {
		case "auto", "default", "flex", "scale", "priority", "fast", "ultrafast":
			return tier, true
		}
		return "", false
	}
	return "", false
}

// InboundCodec 客户端入口协议编解码器。
// 它只负责客户端请求解码，以及把 IR 响应/错误渲染回客户端。
type InboundCodec interface {
	// Name 协议标识，如 "anthropic"、"openai-chat"。
	Name() string

	// DecodeRequest 把客户端请求体解析为 IR 请求。
	DecodeRequest(body []byte) (*ir.Request, error)
	// NewStreamEncoder IR 事件 -> 本协议 SSE 流（客户端下发方向）。
	NewStreamEncoder() StreamEncoder
	// EncodeResponse 把聚合的 IR 响应编码为本协议非流式响应体。
	EncodeResponse(resp *ir.Response) ([]byte, error)
	// ResponseNotes 报告把该响应编码给本协议客户端会发生的损耗
	// （外族签名丢弃、畸形工具参数挪键）。编码前的纯扫描，不产字节；
	// relay 用同一份扫描结果填响应头，与各 codec 的实编行为同源。
	ResponseNotes(resp *ir.Response) []string
	// RenderError 按本协议外形渲染错误响应体与状态码。
	RenderError(e *ir.Error) (status int, body []byte)
	// RenderStreamError 渲染流内错误事件（SSE 字节）。
	RenderStreamError(e *ir.Error) []byte
}

// OutboundCodec 上游出口协议编解码器。
// 它只负责把 IR 请求编码给上游，并把上游响应解码回 IR。
type OutboundCodec interface {
	// Name 协议标识，如 "anthropic"、"openai-chat"。
	Name() string
	// Caps 返回本协议的上游能力声明。
	Caps() Capabilities
	// EncodeRequest 把 IR 请求编码为本协议请求体。
	EncodeRequest(req *ir.Request) ([]byte, error)
	// NewStreamDecoder 本协议 SSE 流 -> IR 事件（上游响应方向）。
	NewStreamDecoder() StreamDecoder
	// DecodeResponse 把上游非流式响应体解析为 IR 响应。
	DecodeResponse(body []byte) (*ir.Response, error)
}

// Codec 一个协议的双向编解码器。
type Codec interface {
	InboundCodec
	OutboundCodec
}

// StreamDecoder 上游 SSE -> IR 事件。
type StreamDecoder interface {
	// Feed 消费一个 SSE 事件（event 名与 data 载荷，OpenAI 系 event 为空）。
	// 返回 0..N 个 IR 事件。data == "[DONE]" 时实现应返回 nil 并标记结束。
	Feed(event, data string) ([]ir.Event, error)
	// Finish 流结束时调用（无论正常结束还是异常断流），
	// 冲刷残余状态（如未补发的 usage、未闭合的工具调用），
	// 保证事件序列以 EvMessageDelta+EvMessageStop 收尾。
	Finish() []ir.Event
}

// DecoderNoteReporter 是流式解码器的可选损耗上报缝。
type DecoderNoteReporter interface {
	Notes() []string
}

// ResponseDecoderWithNotes 是完整响应解码器的可选损耗上报缝。
type ResponseDecoderWithNotes interface {
	DecodeResponseWithNotes(body []byte) (*ir.Response, []string, error)
}

// StreamEncoder IR 事件 -> 客户端 SSE 字节。
type StreamEncoder interface {
	// Encode 把一个 IR 事件编码为 0..N 段已按本协议帧格式的 SSE 字节。
	Encode(ev ir.Event) ([][]byte, error)
	// Finish 流结束冲刷：强制关闭所有打开的 block、补终止事件。
	// 幂等；正常结束后调用应返回空。
	Finish() [][]byte
	// Notes 排干编码过程中累积的响应侧损耗注记（被门控的外族签名等）。
	// 流已经开始后响应头写不了，relay 把注记渲染成 SSE 注释帧收尾。
	Notes() []string
}

// UsageOptIn 是流编码器的可选缝：客户端能显式选择要不要流式 usage 帧的协议
// （目前只有 Chat 的 stream_options.include_usage；其余三族的 usage 是协议
// 内建、无条件回传）。做成可选接口而不是给 NewStreamEncoder 加参数：五族里
// 只有一族需要这个请求侧意图，改签名会让其余四族与装饰器都多带一个恒忽略
// 的参数，六十来处编码器构造点也要跟着动。
type UsageOptIn interface {
	SetIncludeUsage(bool)
}

// NewClientStreamEncoder 建客户端流编码器，并把请求侧的呈现意图下发给支持的
// 协议。客户端流式写出有两个出口（普通流、聚合转流），一律走这里，
// 免得像思考抑制那样逐处判断会漏。req 为 nil 时按各协议的默认意图。
func NewClientStreamEncoder(c InboundCodec, req *ir.Request) StreamEncoder {
	enc := c.NewStreamEncoder()
	if o, ok := enc.(UsageOptIn); ok && req != nil {
		o.SetIncludeUsage(req.IncludeUsage)
	}
	return enc
}

// SSENoteFrames 把响应侧损耗注记渲染为 SSE 注释帧（": " 前缀行）。
// 四个入站协议都是 SSE；注释帧客户端会忽略，但抓包与日志可见——
// 流式方向注记没有别的诚实通道（头已发，事件 schema 里没有注记位）。
func SSENoteFrames(notes []string) [][]byte {
	if len(notes) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(notes))
	for _, n := range notes {
		n = strings.ReplaceAll(n, "\n", " ")
		n = strings.ReplaceAll(n, "\r", " ")
		out = append(out, []byte(": modelsurge-note: "+n+"\n\n"))
	}
	return out
}

// TierEchoDropNote 档位回显丢失注记：流式编码器（越集/无槽位/到得太晚）
// 与非流式 ScanResponseLosses 共用同一措辞。
func TierEchoDropNote(tier string) string {
	return fmt.Sprintf(
		"dropped service tier echo %q: this protocol's response has no equivalent tier value, the client cannot see which capacity tier actually served the request", tier)
}

// ScanResponseLosses 响应侧损耗扫描：编码给客户端前预判会丢什么。
// sigSlotless=协议没有签名槽位（chat，签名全丢）；否则只丢外族签名。
// objArgs=工具参数是对象槽位（anthropic/gemini），非法参数会被挪进
// ir.RawArgsKey；字符串槽位保留原文，但仍报告客户端无法安全执行。
// 与各 codec 的编码分支用同一判定（SignatureGenuineFor / NormalizeToolInput），
// 扫描结果即实编结果。
func ScanResponseLosses(resp *ir.Response, protoName string, sigSlotless, objArgs bool) []string {
	var sigs, badArgs, customCalls int
	uploads, redacted, opaque := 0, 0, 0
	serverCalls, serverResults := 0, 0
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockThinking:
			if b.Thinking != nil && b.Thinking.Signature != "" &&
				(sigSlotless || !b.Thinking.SignatureGenuineFor(protoName)) {
				sigs++
			}
		case ir.BlockRedactedThinking:
			redacted++
		case ir.BlockOpaque:
			opaque++
		case ir.BlockServerToolUse:
			serverCalls++
		case ir.BlockWebSearchToolResult:
			serverResults++
		case ir.BlockToolUse:
			if b.ToolUse != nil {
				if b.ToolUse.Kind == ir.ToolCustom {
					customCalls++
				} else if _, ok := ir.NormalizeToolInput(b.ToolUse.Input); !ok {
					badArgs++
				}
			}
		case ir.BlockContainerUpload:
			uploads++
		}
	}
	var notes []string
	if sigs > 0 {
		if sigSlotless {
			notes = append(notes, fmt.Sprintf(
				"dropped %d thought signature(s): this protocol has no signature slot, the client cannot replay the thinking chain", sigs))
		} else {
			notes = append(notes, fmt.Sprintf(
				"dropped %d thought signature(s): signed by a different protocol family, sending them would fail the client's signature validation", sigs))
		}
	}
	if badArgs > 0 {
		if objArgs {
			notes = append(notes, ir.RewrapNote(badArgs))
		} else {
			notes = append(notes, ir.RawArgsPassNote(badArgs))
		}
	}
	if customCalls > 0 && protoName != "openai-responses" && protoName != "codex" {
		notes = append(notes, CustomToolDowngradeNote(customCalls))
	}
	// 服务档位回显：目标协议值集装不下（或根本没有回显槽位）时，客户端
	// 看不到实际用了哪档容量——计费与延迟预期都对不上。
	if resp.ServiceTier != "" {
		if _, ok := MapServiceTierEcho(resp.ServiceTier, protoName); !ok {
			notes = append(notes, TierEchoDropNote(resp.ServiceTier))
		}
	}
	// 代码执行容器回显是 anthropic 专属：外族响应没有 container 槽位，
	// 客户端拿不到容器 id/过期时间，下一轮无法复用容器续话。
	if resp.Container != nil && protoName != "anthropic" {
		notes = append(notes, ContainerDropNote())
	}
	// container_upload 块同理：外族没有容器文件引用槽位，模型产出/引用
	// 的容器文件整块消失。
	if uploads > 0 && protoName != "anthropic" {
		notes = append(notes, ContainerUploadDropNote(uploads))
	}
	if redacted > 0 && protoName != "anthropic" {
		notes = append(notes, RedactedThinkingDropNote(redacted))
	}
	if opaque > 0 && protoName != "anthropic" {
		notes = append(notes, OpaqueDropNote(opaque))
	}
	if (serverCalls > 0 || serverResults > 0) && protoName != "anthropic" {
		notes = append(notes, ServerToolDropNote(serverCalls, serverResults))
	}
	if resp.Audio != nil && protoName != "openai-chat" {
		notes = append(notes, AudioOutputDropNote())
	}
	if resp.Usage.CacheCreationDetailsKnown && protoName != "anthropic" {
		notes = append(notes, CacheCreationDetailsDropNote())
	}
	if n := countNonPortable(resp.Content); n > 0 && protoName != "anthropic" {
		notes = append(notes, CitationDropNote(n))
	}
	return notes
}

// countNonPortable 统计一批块里目标协议装不下的引用条数。
func countNonPortable(blocks []ir.Block) int {
	n := 0
	for _, b := range blocks {
		n += CountNonPortableCitations(b.Citations)
	}
	return n
}

// CountNonPortableCitations 统计一批引用里带不出本族的条数（用于有损诊断）。
func CountNonPortableCitations(cs []ir.Citation) int {
	n := 0
	for _, c := range cs {
		if !c.Portable() {
			n++
		}
	}
	return n
}

// CitationDropNote 文档类引用丢失注记：非流式扫描、流式编码器与请求侧诊断
// 共用同一措辞。引用的正文与文档标题属会话内容，不进注记。
func CitationDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d document citation(s): this protocol identifies an annotation source by URL, and these citations point at a document index with page/block/character offsets instead, so the client cannot see which passage was cited", n)
}

// ContainerDropNote 容器回显丢失注记：外族无 container 槽位时共用。
func ContainerDropNote() string {
	return "dropped container info: this protocol's response has no container field, the client cannot see or reuse the code-execution container that served the request"
}

// ContainerUploadDropNote 容器文件引用块丢失注记：外族无 container_upload
// 槽位时共用（非流式扫描与流式编码器同一措辞）。
func ContainerUploadDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d container upload block(s): this protocol has no container file-reference slot, the client cannot see files uploaded to or produced by the code-execution container", n)
}

// RedactedThinkingDropNote 涂抹思考块丢失注记：外族没有承载不透明密文的槽位
// （非流式扫描与流式编码器同一措辞）。密文本身不进注记。
func RedactedThinkingDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d redacted thinking block(s): this protocol has no opaque-reasoning slot, the client cannot replay the encrypted thinking state, so a follow-up turn sent to an Anthropic upstream may be rejected", n)
}

// OpaqueDropNote 不透明块丢失注记：块型只在源协议里有定义（Anthropic 的
// web_fetch / code_execution / tool_search 等服务端工具结果、search_result），
// 目标协议没有对应槽位，整块不下发。块体属会话内容，不进注记。
func OpaqueDropNote(n int) string {
	return fmt.Sprintf(
		"dropped %d opaque content block(s): this protocol has no slot for the source protocol's server-side tool payload, the client cannot see the fetched page, command output or search result the model produced", n)
}

// ServerToolDropNote 服务端托管工具块丢失注记。server_tool_use 与
// web_search_tool_result 是**有 IR 块型**的（不同于落进 BlockOpaque 的未知块），
// 三个外族编码器都没有为它们输出任何对应形态：整块消失且原先不计数，客户端
// 既看不到网关代执行了哪次托管搜索，也拿不到搜回来的页面。calls / results 分别
// 是两种块型的条数，只渲染非零的那部分。搜索结果的标题、URL 与摘要属会话内容，
// 不进注记。
//
// 措辞刻意不断言「目标协议没有槽位」：Responses 官方确有 web_search_call 输出项，
// 只是本仓的转换没有实现映射。说的是转换做了什么，不是协议没有什么。
// 也刻意不写「客户端」：这条注记同时用于响应侧（受众是客户端）与请求侧诊断
// （受众是上游模型），用「接收端」才对两个方向都成立。
func ServerToolDropNote(calls, results int) string {
	var subject string
	switch {
	case calls > 0 && results > 0:
		subject = fmt.Sprintf("%d server-side tool call(s) and %d web search result block(s)", calls, results)
	case calls > 0:
		subject = fmt.Sprintf("%d server-side tool call(s)", calls)
	default:
		subject = fmt.Sprintf("%d web search result block(s)", results)
	}
	return "dropped " + subject +
		": this protocol's conversion emits no counterpart for Anthropic's hosted-tool blocks, so the receiving side sees neither which hosted search ran nor which pages it returned, and cannot replay either in a later turn"
}

// AudioOutputDropNote 模型音频输出丢失注记。完整音频只存在于 Chat 非流式
// message.audio；Chat SSE 与所有外族响应都没有等价槽位。
func AudioOutputDropNote() string {
	return "dropped model audio output: this response format has no complete-audio slot, the client cannot play the generated audio or recover its transcript and replay id"
}

func CacheCreationDetailsDropNote() string {
	return "dropped Anthropic cache-creation TTL details: this response format has no 5-minute/1-hour cache-write usage fields; aggregate input token totals remain preserved"
}

func CustomToolDowngradeNote(n int) string {
	return fmt.Sprintf(
		"downgraded %d custom tool call(s) to function calls: this protocol has no free-form tool-input item, input is wrapped as {input:string}", n)
}

func AdditionalChoicesDropNote(n int) string {
	return fmt.Sprintf(
		"discarded %d additional response choice(s): the internal response model carries one candidate, only one OpenAI Chat choice was preserved", n)
}

// SigDropNote 流式编码器的外族签名丢弃注记（计数由编码器在门控分支累计）。
func SigDropNote(n int, sigSlotless bool) string {
	if sigSlotless {
		return fmt.Sprintf(
			"dropped %d thought signature(s): this protocol has no signature slot, the client cannot replay the thinking chain", n)
	}
	return fmt.Sprintf(
		"dropped %d thought signature(s): signed by a different protocol family, sending them would fail the client's signature validation", n)
}

// TruncatedTool 一次被上游截断的工具调用（上游会截断超长工具参数）。
type TruncatedTool struct {
	ID     string
	Name   string // 客户端可见名（别名已还原）
	Reason string // 截断诊断（如 "missing 2 closing brace(s)"）
}

// TruncationReporter 流式解码器可选缝：上报上游截断（Finish 后有效）。
// relay 探测本接口记录截断状态，供下次请求注入恢复提示。
type TruncationReporter interface {
	// TruncatedTools 参数形似被上游截断的工具调用列表。
	TruncatedTools() []TruncatedTool
	// ContentTruncated 流无完成信号但已有正文（且无工具调用）。
	ContentTruncated() bool
	// TruncatedContent 被截断的正文全文（内容哈希关联下次请求用）。
	TruncatedContent() string
}

var (
	inboundRegistry  = map[string]InboundCodec{}
	outboundRegistry = map[string]OutboundCodec{}
)

// Register 注册双向 codec，任一方向重名均 panic（启动期编程错误）。
func Register(c Codec) {
	registerInbound(c)
	registerOutbound(c)
}

// RegisterInbound 仅注册客户端入口 codec。
func RegisterInbound(c InboundCodec) { registerInbound(c) }

func registerInbound(c InboundCodec) {
	if _, dup := inboundRegistry[c.Name()]; dup {
		panic("proto: duplicate inbound codec " + c.Name())
	}
	inboundRegistry[c.Name()] = c
}

func registerOutbound(c OutboundCodec) {
	if _, dup := outboundRegistry[c.Name()]; dup {
		panic("proto: duplicate outbound codec " + c.Name())
	}
	outboundRegistry[c.Name()] = c
}

// GetInbound 按名取客户端入口 codec。
func GetInbound(name string) (InboundCodec, error) {
	c, ok := inboundRegistry[name]
	if !ok {
		return nil, fmt.Errorf("proto: unknown inbound codec %q (have %v)", name, InboundNames())
	}
	return c, nil
}

// MustInbound 按名取入口 codec，不存在则 panic（仅用于测试与启动期装配）。
func MustInbound(name string) InboundCodec {
	c, err := GetInbound(name)
	if err != nil {
		panic(err)
	}
	return c
}

// GetOutbound 按名取上游出口 codec。
func GetOutbound(name string) (OutboundCodec, error) {
	c, ok := outboundRegistry[name]
	if !ok {
		return nil, fmt.Errorf("proto: unknown outbound codec %q (have %v)", name, OutboundNames())
	}
	return c, nil
}

// MustOutbound 按名取出口 codec，不存在则 panic（仅用于测试与启动期装配）。
func MustOutbound(name string) OutboundCodec {
	c, err := GetOutbound(name)
	if err != nil {
		panic(err)
	}
	return c
}

// Names 返回已注册入口协议名。保留该名称用于入口枚举兼容；新代码宜用 InboundNames。
func Names() []string { return InboundNames() }

// InboundNames 返回已注册入口协议名（排序，便于日志与错误信息稳定）。
func InboundNames() []string { return sortedNames(inboundRegistry) }

// OutboundNames 返回已注册出口协议名（排序，便于日志与错误信息稳定）。
func OutboundNames() []string { return sortedNames(outboundRegistry) }

func sortedNames[T any](registry map[string]T) []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
