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
	ThinkingSignature bool // thinking 签名可双向保真（Anthropic signature / Responses encrypted_content / Gemini thoughtSignature）
	Images            bool // 图片输入
	HostedTools       bool // 服务端托管工具声明（web_search / code_execution 等）
}

// Codec 一个协议的双向编解码器。
type Codec interface {
	// Name 协议标识，如 "anthropic"、"openai-chat"。
	Name() string

	// Caps 返回本协议的能力声明。
	Caps() Capabilities

	// DecodeRequest 把客户端/上游的请求体解析为 IR 请求。
	DecodeRequest(body []byte) (*ir.Request, error)
	// EncodeRequest 把 IR 请求编码为本协议请求体。
	// 实现必须保证产出的请求满足本协议上游的结构约束
	// （可调用 normalize 包做消息规整）。
	EncodeRequest(req *ir.Request) ([]byte, error)

	// NewStreamDecoder 本协议 SSE 流 -> IR 事件（上游响应方向）。
	NewStreamDecoder() StreamDecoder
	// NewStreamEncoder IR 事件 -> 本协议 SSE 流（客户端下发方向）。
	NewStreamEncoder() StreamEncoder

	// DecodeResponse 把上游非流式响应体解析为 IR 响应
	// （兜底路径：上游忽略 stream=true 返回完整 JSON 时）。
	DecodeResponse(body []byte) (*ir.Response, error)
	// EncodeResponse 把聚合的 IR 响应编码为本协议非流式响应体
	// （非流式客户端 = 网关聚合上游流后一次性返回）。
	EncodeResponse(resp *ir.Response) ([]byte, error)

	// RenderError 按本协议外形渲染错误响应体与状态码。
	RenderError(e *ir.Error) (status int, body []byte)
	// RenderStreamError 渲染流内错误事件（SSE 字节）。
	RenderStreamError(e *ir.Error) []byte
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

var registry = map[string]Codec{}

// Register 注册 codec，重名 panic（启动期编程错误）。
func Register(c Codec) {
	if _, dup := registry[c.Name()]; dup {
		panic("proto: duplicate codec " + c.Name())
	}
	registry[c.Name()] = c
}

// Get 按名取 codec。
func Get(name string) (Codec, error) {
	c, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("proto: unknown codec %q (have %v)", name, Names())
	}
	return c, nil
}

// Must 按名取 codec，不存在则 panic（仅用于测试与启动期装配）。
func Must(name string) Codec {
	c, err := Get(name)
	if err != nil {
		panic(err)
	}
	return c
}

// Names 返回已注册协议名（排序，便于日志与错误信息稳定）。
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
