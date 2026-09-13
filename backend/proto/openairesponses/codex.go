package openairesponses

import "relayd/backend/proto"

// NameCodex Codex 订阅端点协议名（chatgpt.com/backend-api/codex，ChatGPT OAuth）。
// 请求/响应形态与官方 Responses API 完全一致，仅身份面不同：
// 端点路径 /responses（无 /v1）与身份头（originator/User-Agent/version 等）
// 由 relay 端点层按协议名处理；instructions 字段恒输出空串兜底。
const NameCodex = "codex"

// codexCodec openai-responses 的同形别名 codec。
type codexCodec struct {
	codec
}

func (codexCodec) Name() string { return NameCodex }

func init() { proto.Register(codexCodec{codec{}}) }
