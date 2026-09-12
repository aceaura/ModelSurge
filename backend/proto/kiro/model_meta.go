// Package kiro 实现 Kiro 上游协议 codec（IR ↔ generateAssistantResponse
// eventstream），与 anthropic / openai-chat / openai-responses / gemini 平级。
// 本文件：模型元数据常量（KiroaaS config.py 的模型表 + model_id_format 的
// 点/横线展示转换）。
package kiro

import "strings"

// FALLBACK_MODELS /ListAvailableModels 不可达时的静态兜底表
// （代表本版本已知的模型；不代表账号计划一定可用）。
var FALLBACK_MODELS = []string{
	"auto",
	"claude-sonnet-4",
	"claude-sonnet-4.5",
	"claude-sonnet-4.6",
	"claude-haiku-4.5",
	"claude-opus-4.5",
	"claude-opus-4.6",
	"claude-opus-4.7",
	"claude-opus-4.8",
	"claude-opus-5",
	"claude-sonnet-5",
	"gpt-5.5",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"deepseek-3.2",
	"glm-5",
	"minimax-m2.1",
	"minimax-m2.5",
	"qwen3-coder-next",
}

// MODEL_ALIASES 别名 -> 真实模型 ID（避免与 IDE 专属模型名冲突，如 Cursor 的 auto）。
var MODEL_ALIASES = map[string]string{
	"auto-kiro": "auto",
}

// HIDDEN_MODELS /ListAvailableModels 不返回但仍可用的模型（展示名 -> 内部 ID）。
var HIDDEN_MODELS = map[string]string{}

// HIDDEN_FROM_LIST /v1/models 不展示（仍可直呼）的模型 ID。
var HIDDEN_FROM_LIST = []string{"auto"}

// DefaultMaxInputTokens 模型条目缺 tokenLimits 时的默认输入上限。
const DefaultMaxInputTokens = 200000

// DashifyClaudeID 点号 Claude ID -> 横线形态（/v1/models 展示用：
// Claude Code / Claude Desktop 只认横线形态；请求侧 normalize 会转回点号，
// 两个方向等价解析）。非 Claude ID 原样返回。
func DashifyClaudeID(modelID string) string {
	if !strings.Contains(strings.ToLower(modelID), "claude") {
		return modelID
	}
	return strings.ReplaceAll(modelID, ".", "-")
}
