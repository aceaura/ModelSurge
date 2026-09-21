// Package proto 定义协议 codec 接口与注册表。
// 每个协议（anthropic / openai-chat / openai-responses / gemini）实现一个 Codec，
// 通过 Register 注册；跨协议转换经由 ir 中转，协议包之间互不依赖。
package proto

import (
	"fmt"
	"sort"

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
	// responseMimeType + responseSchema。Anthropic 与 kiro 的载荷里没有这一维：
	// 官方做法是把 schema 写进 system 提示或声明单工具后强制调用，两者都在改写
	// 请求语义，本层不做——客户端拿到的会是自由文本，JSON.parse 会失败。
	StructuredOutput bool

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
