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
	// 四个出站都能收 base64，只有 kiro 收不了 URL（它把 URL 形态静默跳过）。
	// 合用一位会让「整个协议没有图片能力」与「只是这一种形态装不下」同形，
	// 而前者从不发生——四个 codec 的 Images 全为真，那条诊断分支恒假。
	ImageURLs bool

	// Sampling 采样参数（temperature / top_p / stop_sequences / max_tokens）
	// 能随请求送达上游。kiro 的载荷里没有这些字段，客户端调的参数全部无效。
	Sampling bool

	// TopK 独立于 Sampling：只有 Anthropic 与 Gemini 有这一维，
	// OpenAI 两系原生没有对应字段，不是「能力缺失」而是协议里就不存在。
	TopK bool

	// ParallelToolCalls 可表达「禁止并行工具调用」。Anthropic 是
	// disable_parallel_tool_use，Chat 与 responses 都是 parallel_tool_calls；
	// 只有 kiro 的载荷里没有这一维。
	ParallelToolCalls bool

	// StructuredOutput 结构化输出约束（JSON 模式 / JSON Schema）能随请求送达。
	// Chat 是 response_format，Responses 是 text.format，Gemini 是
	// responseMimeType + responseSchema，Anthropic 是 output_config.format
	// （2026 年新增）。只有 kiro 的载荷里没有这一维：官方做法是把 schema
	// 写进 system 提示或声明单工具后强制调用，两者都在改写请求语义，本层
	// 不做——客户端拿到的会是自由文本，JSON.parse 会失败。
	StructuredOutput bool

	// StructuredOutputSchemaOnly 结构化输出只接 schema 约束形态，纯 JSON
	// 模式（只要求合法 JSON、不给 schema）没有槽位。Anthropic 的
	// output_config.format 只有 json_schema 一种 type，是唯一的受限者；
	// 其余三家两种形态都能表达。
	StructuredOutputSchemaOnly bool

	// Documents PDF 等文档附件输入。Anthropic 是 document 块，Chat 是 file 部分，
	// Responses 是 input_file，Gemini 是 inlineData（MIME 白名单含 application/pdf）。
	// 只有 kiro 的载荷里没有任何文档槽位。
	Documents bool

	// Audio 音频附件输入。只有 Chat 的 input_audio、Responses 的 input_audio 与
	// Gemini 的 inlineData 有；Anthropic 与 kiro 完全没有音频入口。
	// 与 Documents 分开是因为 Anthropic 能收文档但收不了音频，合位会把两者的
	// 诊断结论弄反，而读者的下一步动作不同（转文字 vs 保留原附件）。
	Audio bool

	// Video 视频附件输入。只有 Gemini 有原生槽位。
	Video bool

	// Refusal 有独立的「模型拒绝作答」槽位。OpenAI 两系有（Chat 的
	// message.refusal、Responses 的 refusal content part），Anthropic、Gemini
	// 与 kiro 没有——那三家只有 stop_reason/finishReason 能表达「这是拒绝」，
	// 正文只能并入普通文本。装不下时降级为文本而非丢弃：拒绝正文是模型真正
	// 说出的话，丢了客户端只剩一条空消息。
	Refusal bool

	// ToolResultError 工具结果能标出「这次调用失败了」。Anthropic 是
	// is_error，kiro 是 status=error，Gemini 靠 response 里的 error 键约定。
	// OpenAI 两系的 tool / function_call_output 里没有任何这类标志：失败结果
	// 与成功结果同形，模型只能从文本自行猜测。
	ToolResultError bool

	// Citations 正文的来源标注有槽位。Anthropic 是 text.citations，Chat 是
	// message.annotations，Responses 是 output_text.annotations，Gemini 是
	// groundingMetadata；只有 kiro 的载荷里没有任何位置。装不下时正文照常送达，
	// 丢的是「这句话出自哪里」——客户端会把有出处的结论渲染成模型的自由发挥。
	Citations bool

	// 以下是调参维度的承载能力。为假时照常发请求、只出诊断说明：拒绝会把一个
	// 能用的回答换成零回答，而目标协议是调度层按策略选的、客户端无从预知，
	// 让它为一个自己控制不了的路由结果吃 400，故障归因方向是错的。

	// Penalties presence_penalty / frequency_penalty。Chat 与 Gemini 有，
	// Anthropic、Responses 与 kiro 的载荷里没有这一维。
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
	// Gemini args、kiro input）而非字符串形态（Chat/Responses arguments）。
	// 对象槽位装不下非法/非对象参数，会被规整进 ir.RawArgsKey 键位；
	// 字符串槽位原样透传。两者诊断措辞不同，读者要改的地方也不同。
	ToolInputObject bool

	// UserID 有终端用户标识槽位（Anthropic metadata.user_id、
	// Chat/Responses 的 user）。kiro 没有这一维。
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

// ScanResponseLosses 响应侧损耗扫描：编码给客户端前预判会丢什么。
// sigSlotless=协议没有签名槽位（chat，签名全丢）；否则只丢外族签名。
// objArgs=工具参数是对象槽位（anthropic/gemini/kiro），非法参数会被挪进
// ir.RawArgsKey；字符串槽位原样透传无损耗。
// 与各 codec 的编码分支用同一判定（SignatureGenuineFor / NormalizeToolInput），
// 扫描结果即实编结果。
func ScanResponseLosses(resp *ir.Response, protoName string, sigSlotless, objArgs bool) []string {
	var sigs, badArgs int
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockThinking:
			if b.Thinking != nil && b.Thinking.Signature != "" &&
				(sigSlotless || !b.Thinking.SignatureGenuineFor(protoName)) {
				sigs++
			}
		case ir.BlockToolUse:
			if objArgs && b.ToolUse != nil {
				if _, ok := ir.NormalizeToolInput(b.ToolUse.Input); !ok {
					badArgs++
				}
			}
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
		notes = append(notes, ir.RewrapNote(badArgs))
	}
	return notes
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

// TruncatedTool 一次被上游截断的工具调用（kiro 上游会截断大工具参数）。
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
