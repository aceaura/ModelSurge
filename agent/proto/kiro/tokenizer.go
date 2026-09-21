// tokenizer.go count_tokens 估算（tokenizer.py 的 Go 翻译，输入侧换成 IR）。
// Python 用 tiktoken cl100k_base × 1.15 修正系数（Claude 比 GPT-4 多 ~15%
// token），tiktoken 不可用时退化为 len/4 粗估；Go 侧不内嵌 BPE 词表，
// 文本级估算改为 ASCII 4 字符/token + 非 ASCII 1 字/token（对 CJK 远比
// len/4 接近真实，Python 注释自认该兜底对非英文显著低估）。
// 结构语义保持一致：消息/工具/系统提示分别计数、每消息 ~4 服务 token、
// 结尾 +3、图片按 100 粗估、总量 × 1.15。
package kiro

import (
	"encoding/json"

	"github.com/aceaura/ModelSurge/agent/ir"
)

// claudeCorrectionFactor Claude 相对 cl100k_base 的经验修正系数
// （tokenizer.py CLAUDE_CORRECTION_FACTOR）。
const claudeCorrectionFactor = 1.15

// imageTokenCost 图片块固定粗估（避免明显低估；tokenizer.py 同值）。
const imageTokenCost = 100

// countTokens 文本 token 估算。correction 为 true 时应用 Claude 修正
// （Python 内部逐块计数均不修正，聚合后统一修正）。
func countTokens(s string, correction bool) int {
	if s == "" {
		return 0
	}
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	n := ascii/4 + nonASCII
	if correction {
		return int(float64(n) * claudeCorrectionFactor)
	}
	return n
}

// countMessageTokens 消息列表 token 估算（IR 消息形态）：
// 每消息 ~4 服务 token + role + 内容块；tool_use 计 id/name/input JSON，
// tool_result 计 tool_use_id/is_error/内容块；未知块按 JSON 序列化兜底。
func countMessageTokens(req *ir.Request) int {
	total := 0
	for _, m := range req.Messages {
		total += 4 // 消息结构开销（role、分隔符）
		total += countTokens(string(m.Role), false)
		for _, b := range m.Content {
			total += countBlockTokens(b)
		}
	}
	return total + 3 // 结尾服务 token
}

// countBlockTokens 单个内容块 token 估算。
func countBlockTokens(b ir.Block) int {
	switch b.Type {
	case ir.BlockText, ir.BlockRefusal:
		// 拒绝正文同样要计费：漏计会让含拒绝历史的请求低估上下文，
		// 客户端据此判断还能塞多少内容，估少了会直接超限。
		return countTokens(b.Text, false)
	case ir.BlockThinking:
		if b.Thinking != nil {
			return countTokens(b.Thinking.Text, false)
		}
		return 0
	case ir.BlockImage:
		return imageTokenCost
	case ir.BlockToolUse:
		if b.ToolUse == nil {
			return 0
		}
		return countTokens(b.ToolUse.ID, false) +
			countTokens(b.ToolUse.Name, false) +
			countTokens(string(b.ToolUse.Input), false)
	case ir.BlockToolResult:
		if b.ToolResult == nil {
			return 0
		}
		n := countTokens(b.ToolResult.ToolUseID, false)
		if b.ToolResult.IsError {
			n += countTokens("true", false)
		}
		for _, cb := range b.ToolResult.Content {
			switch cb.Type {
			case ir.BlockImage:
				n += imageTokenCost
			default: // text
				n += countTokens(cb.Text, false)
			}
		}
		return n
	default: // server_tool_use / web_search_tool_result 等未知块按 JSON 兜底
		if raw, err := json.Marshal(b); err == nil {
			return countTokens(string(raw), false)
		}
		return 0
	}
}

// countToolsTokens 工具定义 token 估算：每工具 ~4 服务 token +
// name + description + schema JSON（tokenizer.py count_tools_tokens）。
func countToolsTokens(tools []ir.Tool) int {
	total := 0
	for _, t := range tools {
		total += 4
		total += countTokens(t.Name, false)
		total += countTokens(t.Description, false)
		total += countTokens(string(t.InputSchema), false)
	}
	return total
}

// countSystemTokens 系统提示 token 估算（文本块；cache_control 开销小，略）。
func countSystemTokens(system []ir.Block) int {
	total := 0
	for _, b := range system {
		total += countTokens(b.Text, false)
	}
	return total
}

// EstimateRequestTokens 按 kiro tokenizer 语义估算请求输入 token
// （tokenizer.py estimate_request_tokens）：消息 + 工具 + 系统，
// 总量应用 Claude 修正系数。kiro 账号命中 count_tokens 时本地返回。
func (Codec) EstimateRequestTokens(req *ir.Request) int {
	total := countMessageTokens(req) + countToolsTokens(req.Tools) + countSystemTokens(req.System)
	return int(float64(total) * claudeCorrectionFactor)
}
